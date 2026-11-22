package main

// 设备遥控
//
// 现状：只能「绑定设备」，绑完就断了——手机端没法让音箱切歌/调音量，
// 音箱也没法主动问服务器「现在该放什么」。
//
// 这里加一条极简的指令通道（设备侧用长轮询拿指令，不需要公网回调、
// 不需要固件开端口）：
//
//	手机/Web  --POST /api/device/command-->  服务器内存队列
//	ESP32     --GET  /api/esp32/poll?wait=20-->  有指令立即返回，没有就挂 20s
//	ESP32     --POST /api/esp32/ack-------->  回报执行结果
//
// 指令只在内存里排队，未取走 5 分钟后自动过期；同一设备最多积压 50 条。
// 目前的固件不认这条通道，所以对现有设备零影响（它不轮询就不会有变化）。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type DeviceCommand struct {
	ID        string                 `json:"id"`
	Action    string                 `json:"action"`
	Params    map[string]interface{} `json:"params,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	ExpiresAt time.Time              `json:"expires_at"`
}

type deviceCommandAck struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	At     string `json:"at"`
}

const (
	commandTTL         = 5 * time.Minute
	commandQueueMax    = 50
	commandMaxWaitSec  = 55
	commandDefaultWait = 20
)

type deviceCommandQueue struct {
	mu      sync.Mutex
	pending map[string][]*DeviceCommand
	signal  map[string]chan struct{}
	acks    map[string]deviceCommandAck // mac -> 最近一次回报
}

var deviceCommands = &deviceCommandQueue{
	pending: make(map[string][]*DeviceCommand),
	signal:  make(map[string]chan struct{}),
	acks:    make(map[string]deviceCommandAck),
}

// 指令白名单：设备端只认这些动作，服务端也挡一层，避免网页端被诱导下发任意指令。
var allowedCommands = map[string]bool{
	"play": true, "pause": true, "resume": true, "stop": true,
	"next": true, "prev": true, "volume": true, "seek": true,
	"play_song": true, "play_playlist": true, "reload_playlist": true,
	"set_loop": true, "set_shuffle": true, "reboot": true,
}

func (q *deviceCommandQueue) signalChanLocked(mac string) chan struct{} {
	if ch, ok := q.signal[mac]; ok {
		return ch
	}
	ch := make(chan struct{})
	q.signal[mac] = ch
	return ch
}

func (q *deviceCommandQueue) push(mac string, cmd *DeviceCommand) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	list := append(q.pending[mac], cmd)
	if len(list) > commandQueueMax {
		list = list[len(list)-commandQueueMax:]
	}
	q.pending[mac] = list

	// 唤醒正在长轮询的设备：关掉旧 channel，换一个新的给下一次等待
	if ch, ok := q.signal[mac]; ok {
		close(ch)
	}
	q.signal[mac] = make(chan struct{})
	return len(list)
}

func (q *deviceCommandQueue) drainLocked(mac string) []*DeviceCommand {
	list := q.pending[mac]
	if len(list) == 0 {
		return nil
	}
	q.pending[mac] = nil

	now := time.Now()
	out := make([]*DeviceCommand, 0, len(list))
	for _, c := range list {
		if now.Before(c.ExpiresAt) {
			out = append(out, c)
		}
	}
	return out
}

// wait 取指令：已有指令立即返回；否则最多挂 wait 秒。
func (q *deviceCommandQueue) wait(mac string, wait time.Duration) []*DeviceCommand {
	q.mu.Lock()
	ch := q.signalChanLocked(mac)
	q.mu.Unlock()

	if cmds := q.drain(mac); len(cmds) > 0 {
		return cmds
	}
	if wait <= 0 {
		return nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	}
	return q.drain(mac)
}

func (q *deviceCommandQueue) drain(mac string) []*DeviceCommand {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.drainLocked(mac)
}

func (q *deviceCommandQueue) pendingCount(mac string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending[mac])
}

func (q *deviceCommandQueue) setAck(mac string, ack deviceCommandAck) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.acks[mac] = ack
}

func (q *deviceCommandQueue) getAck(mac string) deviceCommandAck {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.acks[mac]
}

func newCommandID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// deviceFromRequest 用 X-Device-Token（或 ?token=）鉴别设备身份。
func deviceFromRequest(r *http.Request) (*Device, error) {
	token := strings.TrimSpace(r.Header.Get("X-Device-Token"))
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if token == "" {
		return nil, fmt.Errorf("缺少设备 Token")
	}
	return GetDeviceManager().VerifyToken(token)
}

// HandleDeviceCommand Web 端下发指令（需要登录，且只能操作自己绑定的设备）。
func HandleDeviceCommand(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持 POST"})
		return
	}

	username := GetCurrentUser(r)
	if username == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "未登录"})
		return
	}

	var req struct {
		MAC    string                 `json:"mac"`
		Action string                 `json:"action"`
		Params map[string]interface{} `json:"params"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求格式错误"})
		return
	}

	mac := strings.TrimSpace(req.MAC)
	action := strings.TrimSpace(req.Action)
	if mac == "" || action == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "缺少 mac 或 action"})
		return
	}
	if !allowedCommands[action] {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "不支持的指令: " + action})
		return
	}

	dev, err := GetDeviceManager().GetDeviceByMAC(mac)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "设备未绑定"})
		return
	}
	if !strings.EqualFold(dev.Username, username) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "该设备不属于当前账号"})
		return
	}

	now := time.Now()
	cmd := &DeviceCommand{
		ID:        newCommandID(),
		Action:    action,
		Params:    req.Params,
		CreatedAt: now,
		ExpiresAt: now.Add(commandTTL),
	}
	queued := deviceCommands.push(mac, cmd)
	fmt.Printf("[Device] 下发指令 %s -> %s (%s)，队列 %d\n", action, mac, cmd.ID, queued)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"id":      cmd.ID,
		"queued":  queued,
		"note":    "指令已入队，设备下次轮询（/api/esp32/poll）时取走",
	})
}

// HandleEsp32Poll 设备端长轮询取指令。
func HandleEsp32Poll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	dev, err := deviceFromRequest(r)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	mac := strings.TrimSpace(r.URL.Query().Get("mac"))
	if mac == "" {
		mac = dev.MAC
	}
	if !strings.EqualFold(mac, dev.MAC) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "MAC 与 Token 不匹配"})
		return
	}

	waitSec := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("wait")); raw != "" {
		if n, e := strconv.Atoi(raw); e == nil {
			waitSec = n
		}
	} else {
		waitSec = commandDefaultWait
	}
	if waitSec < 0 {
		waitSec = 0
	}
	if waitSec > commandMaxWaitSec {
		waitSec = commandMaxWaitSec
	}

	GetDeviceManager().UpdateLastSeen(mac)
	cmds := deviceCommands.wait(mac, time.Duration(waitSec)*time.Second)
	// nil slice 会被编码成 null，固件/脚本都得多判一次；统一成空数组更好处理
	if cmds == nil {
		cmds = []*DeviceCommand{}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"mac":         mac,
		"commands":    cmds,
		"server_time": time.Now().Unix(),
	})
}

// HandleEsp32Ack 设备回报指令执行结果（只用于排障展示，不影响主流程）。
func HandleEsp32Ack(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	dev, err := deviceFromRequest(r)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	var req struct {
		ID    string `json:"id"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	json.Unmarshal(body, &req)

	deviceCommands.setAck(dev.MAC, deviceCommandAck{
		ID:    req.ID,
		OK:    req.OK,
		Error: req.Error,
		At:    time.Now().Format("15:04:05"),
	})
	if !req.OK {
		fmt.Printf("[Device] %s 执行 %s 失败: %s\n", dev.MAC, req.ID, req.Error)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// HandleDeviceCommandStatus 给 Web 控制页显示队列与最近一次执行结果。
func HandleDeviceCommandStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	username := GetCurrentUser(r)
	if username == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "未登录"})
		return
	}

	mac := strings.TrimSpace(r.URL.Query().Get("mac"))
	devices := GetDeviceManager().GetUserDevices(username)
	out := make([]map[string]interface{}, 0, len(devices))
	for _, d := range devices {
		if mac != "" && !strings.EqualFold(d.MAC, mac) {
			continue
		}
		ack := deviceCommands.getAck(d.MAC)
		out = append(out, map[string]interface{}{
			"mac":           d.MAC,
			"device_name":   d.DeviceName,
			"last_seen":     d.LastSeen,
			"online":        time.Since(d.LastSeen) < 3*time.Minute,
			"pending":       deviceCommands.pendingCount(d.MAC),
			"last_command":  ack.ID,
			"last_ok":       ack.OK,
			"last_error":    ack.Error,
			"last_ack_time": ack.At,
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "devices": out})
}
