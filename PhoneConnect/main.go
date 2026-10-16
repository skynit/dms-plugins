package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/creack/pty"
	"github.com/hashicorp/mdns"
)

type device struct {
	Host   string `json:"host"`
	Port   string `json:"port"`
	Name   string `json:"name,omitempty"`
	Serial string `json:"serial,omitempty"`
}

type options struct {
	ScreenOff    bool   `json:"screenOff"`
	StayAwake    bool   `json:"stayAwake"`
	KeepActive   bool   `json:"keepActive"`
	NoPowerOn    bool   `json:"noPowerOn"`
	KeyboardUHID bool   `json:"keyboardUhid"`
	Audio        string `json:"audio"`
	Record       bool   `json:"record"`
	RecordName   string `json:"recordName"`
	TimeLimit    string `json:"timeLimit"`
}

type request struct {
	Host    string   `json:"host"`
	Port    string   `json:"port"`
	Code    string   `json:"code"`
	Path    string   `json:"path"`
	Paths   []string `json:"paths"`
	Options options  `json:"options"`
}

type upload struct {
	Path string
	Name string
	Size int64
}

type result struct {
	Text string
	Err  string
	Code int
}

var home, _ = os.UserHomeDir()
var stateDir = func() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "dms-phone-connect")
	}
	return filepath.Join(home, ".local", "state", "dms-phone-connect")
}()
var browse = discover
var execute = run
var start = launch
var transfer = runTransfer
var pause = time.Sleep

const downloadDir = "/sdcard/Download/"

func address(host, port string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, "/:; \\@") {
		return "", errors.New("请输入有效的 IPv4 地址或主机名")
	}
	if ip := net.ParseIP(host); ip == nil {
		if len(host) > 253 || !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*$`).MatchString(host) {
			return "", errors.New("请输入有效的 IPv4 地址或主机名")
		}
		if regexp.MustCompile(`^[0-9.]+$`).MatchString(host) {
			return "", errors.New("请输入有效的 IPv4 地址或主机名")
		}
	} else if ip.To4() == nil {
		return "", errors.New("请输入有效的 IPv4 地址或主机名")
	}
	n, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("请输入 1–65535 的端口")
	}
	return fmt.Sprintf("%s:%d", host, n), nil
}

func run(timeout time.Duration, input string, args ...string) result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return result{Err: args[0] + " 不可用或等待超时", Code: 1}
	}
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return result{Err: args[0] + " 不可用或等待超时", Code: 1}
		}
		return result{Text: stdout.String(), Err: stderr.String(), Code: 1}
	}
	return result{Text: stdout.String(), Err: stderr.String()}
}

func serviceDevice(service string, entry *mdns.ServiceEntry) (device, bool) {
	if entry == nil || entry.AddrV4 == nil || entry.Port < 1 || !strings.HasSuffix(entry.Name, "."+service+".local.") {
		return device{}, false
	}
	host, port := entry.AddrV4.String(), strconv.Itoa(entry.Port)
	if _, err := address(host, port); err != nil {
		return device{}, false
	}
	name := strings.TrimSuffix(entry.Name, "."+service+".local.")
	label := name
	for _, field := range entry.InfoFields {
		if value, ok := strings.CutPrefix(field, "name="); ok {
			label = value
			break
		}
	}
	return device{Host: host, Port: port, Name: label, Serial: name + "." + service}, true
}

func discover(service string) []device {
	entries := make(chan *mdns.ServiceEntry, 32)
	found := []device{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for entry := range entries {
			item, valid := serviceDevice(service, entry)
			if !valid {
				continue
			}
			if slices.ContainsFunc(found, func(other device) bool { return other.Host == item.Host && other.Port == item.Port }) {
				continue
			}
			found = append(found, item)
		}
	}()
	_ = mdns.QueryContext(context.Background(), &mdns.QueryParam{
		Service: service, Domain: "local", Timeout: 4 * time.Second,
		Entries: entries, DisableIPv6: true, Logger: log.New(io.Discard, "", 0),
	})
	close(entries)
	<-done
	return found
}

func devices() ([]string, error) {
	r := execute(8*time.Second, "", "adb", "devices")
	if r.Code != 0 {
		return nil, errors.New(first(r.Err, "无法读取 ADB 设备"))
	}
	var serials []string
	for _, line := range strings.Split(r.Text, "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == "device" {
			serials = append(serials, parts[0])
		}
	}
	return serials, nil
}

func first(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func connected() (bool, error) {
	serials, err := devices()
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(serials, func(serial string) bool { return !strings.HasPrefix(serial, "emulator-") }), nil
}

func file(name string) string { return filepath.Join(stateDir, name+".json") }

func read(name string) *device {
	data, err := os.ReadFile(file(name))
	if err != nil {
		return nil
	}
	var value device
	if json.Unmarshal(data, &value) != nil {
		return nil
	}
	if value.Serial == "" || value.Host != "" || value.Port != "" {
		if _, err := address(value.Host, value.Port); err != nil {
			return nil
		}
	}
	return &value
}

func write(name string, value device) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(stateDir, ".device-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(data); err == nil {
		err = temp.Close()
	} else {
		temp.Close()
	}
	if err != nil {
		return err
	}
	return os.Rename(temp.Name(), file(name))
}

func save(value device) error {
	if err := write("device", value); err != nil {
		return err
	}
	_ = os.Remove(file("ignored"))
	return nil
}

func known() *device {
	serials, err := devices()
	if err != nil {
		return nil
	}
	var found []device
	var wireless []string
	for _, serial := range serials {
		if strings.HasSuffix(serial, "._adb-tls-connect._tcp") {
			wireless = append(wireless, serial)
			continue
		}
		idx := strings.LastIndexByte(serial, ':')
		if idx < 0 {
			continue
		}
		host, port := serial[:idx], serial[idx+1:]
		if _, err := address(host, port); err == nil {
			found = append(found, device{Host: host, Port: port, Serial: serial})
		}
	}
	if len(wireless) > 0 {
		seen := make(map[string]bool)
		for _, item := range browse("_adb-tls-connect._tcp") {
			if slices.Contains(wireless, item.Serial) {
				found = append(found, item)
				seen[item.Serial] = true
			}
		}
		for _, serial := range wireless {
			if !seen[serial] {
				return nil
			}
		}
	}
	unique := make(map[string]device)
	for _, item := range found {
		unique[item.Host+":"+item.Port] = item
	}
	if len(unique) != 1 {
		return nil
	}
	for _, item := range unique {
		if ignored := read("ignored"); ignored != nil {
			if (ignored.Serial != "" && ignored.Serial == item.Serial) || (ignored.Host != "" && ignored.Host == item.Host) {
				return nil
			}
		}
		if err := save(item); err == nil {
			return read("device")
		}
	}
	return nil
}

func command(serial string, opt options, videos string) ([]string, string, error) {
	args := []string{"scrcpy", "-s", serial}
	for _, flag := range []struct {
		enabled bool
		name    string
	}{{opt.ScreenOff, "-S"}, {opt.StayAwake, "-w"}, {opt.KeepActive, "--keep-active"},
		{opt.NoPowerOn, "--no-power-on"}, {opt.KeyboardUHID, "-K"}} {
		if flag.enabled {
			args = append(args, flag.name)
		}
	}
	switch opt.Audio {
	case "", "default":
	case "duplicate":
		args = append(args, "--audio-source=playback", "--audio-dup")
	case "none":
		args = append(args, "--no-audio")
	default:
		return nil, "", errors.New("音频设置无效")
	}
	if !opt.Record {
		return args, "", nil
	}
	name := strings.TrimSpace(first(opt.RecordName, "phone.mp4"))
	ext := strings.ToLower(filepath.Ext(name))
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\") || utf8.RuneCountInString(name) > 100 ||
		strings.ContainsFunc(name, unicode.IsControl) || (ext != ".mp4" && ext != ".mkv") {
		return nil, "", errors.New("录制文件名只能是 .mp4 或 .mkv 文件名，不能包含路径")
	}
	if videos == "" {
		videos = filepath.Join(home, "Videos")
	}
	path := filepath.Join(videos, name)
	for index := 2; ; index++ {
		_, err := os.Lstat(path)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return nil, "", err
		}
		path = filepath.Join(videos, strings.TrimSuffix(name, filepath.Ext(name))+"-"+strconv.Itoa(index)+filepath.Ext(name))
	}
	args = append(args, "--record="+path)
	switch opt.TimeLimit {
	case "", "0":
	case "600", "1800", "3600":
		args = append(args, "--time-limit="+opt.TimeLimit)
	default:
		return nil, "", errors.New("录制时长设置无效")
	}
	return args, path, nil
}

func launch(serial string, opt options) (string, error) {
	args, path, err := command(serial, opt, "")
	if err != nil {
		return "", err
	}
	if path != "" {
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", errors.New("无法创建录制目录 ~/Videos")
		}
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer null.Close()
	output, err := scrcpyLog()
	if err != nil {
		return "", err
	}
	defer output.Close()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, output, output
	cmd.Env = append(os.Environ(), "SDL_VIDEODRIVER=wayland")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", errors.New("找不到 scrcpy，请先安装")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return "", errors.New("scrcpy 启动失败，请在终端运行 scrcpy 查看原因")
	case <-time.After(350 * time.Millisecond):
		return path, nil
	}
}

func scrcpyLog() (*os.File, error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, errors.New("无法创建状态目录")
	}
	return os.OpenFile(filepath.Join(stateDir, "scrcpy.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
}

var percentPattern = regexp.MustCompile(`\[\s*(\d{1,3})%\]`)
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

func cleanLine(text string) string { return strings.TrimSpace(ansiPattern.ReplaceAllString(text, "")) }

func localFile(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("请选择要推送的文件")
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("请选择有效的本机文件")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("只能推送普通文件，不能推送目录或符号链接")
	}
	return path, nil
}

func localFiles(paths []string) ([]upload, error) {
	if len(paths) == 0 {
		return nil, errors.New("请选择要推送的文件")
	}
	if len(paths) > 50 {
		return nil, errors.New("一次最多推送 50 个文件")
	}
	uploads := make([]upload, 0, len(paths))
	seen := make(map[string]bool)
	names := make(map[string]bool)
	for _, raw := range paths {
		path, err := localFile(raw)
		if err != nil {
			return nil, err
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		info, err := os.Stat(path)
		if err != nil {
			return nil, errors.New("无法读取本机文件 " + filepath.Base(path))
		}
		name := filepath.Base(path)
		if names[name] {
			return nil, errors.New("选中的文件里有重名文件（" + name + "），改文件名后再试")
		}
		names[name] = true
		uploads = append(uploads, upload{Path: path, Name: name, Size: info.Size()})
	}
	if len(uploads) == 0 {
		return nil, errors.New("请选择要推送的文件")
	}
	return uploads, nil
}

func overallPercent(uploads []upload, index, filePercent int) int {
	if len(uploads) == 0 {
		return 0
	}
	if index < 0 {
		index = 0
	}
	if index >= len(uploads) {
		index = len(uploads) - 1
	}
	var total, done int64
	for _, item := range uploads {
		total += item.Size
	}
	for position := 0; position < index; position++ {
		done += uploads[position].Size
	}
	if total <= 0 {
		return (index*100 + filePercent) / len(uploads)
	}
	done += uploads[index].Size * int64(filePercent) / 100
	return int(done * 100 / total)
}

func activeSerial() (string, error) {
	value, err := activeDevice()
	if err != nil {
		return "", err
	}
	if value == nil {
		return "", errors.New("没有已连接的手机，请先连接")
	}
	return value.Serial, nil
}

func activeDevice() (*device, error) {
	serials, err := devices()
	if err != nil {
		return nil, err
	}
	usable := make([]device, 0, len(serials))
	for _, serial := range serials {
		if strings.HasPrefix(serial, "emulator-") {
			continue
		}
		value := device{Serial: serial}
		if host, port, err := net.SplitHostPort(serial); err == nil {
			if _, err := address(host, port); err == nil {
				value.Host, value.Port = host, port
			}
		}
		usable = append(usable, value)
	}
	if len(usable) == 0 {
		return nil, nil
	}
	if value := read("device"); value != nil {
		for _, item := range usable {
			if value.Serial != "" && item.Serial == value.Serial {
				return &item, nil
			}
		}
		for _, item := range usable {
			if item.Host != "" && item.Host == value.Host && item.Port == value.Port {
				return &item, nil
			}
		}
	}
	if len(usable) == 1 {
		return &usable[0], nil
	}
	wireless := slices.ContainsFunc(usable, func(item device) bool {
		return strings.HasSuffix(item.Serial, "._adb-tls-connect._tcp")
	})
	addresses := slices.ContainsFunc(usable, func(item device) bool { return item.Host != "" })
	if wireless && addresses {
		// 同一台手机可能同时以地址和无线服务名称出现在 ADB 列表中。
		services := browse("_adb-tls-connect._tcp")
		unique := make(map[string]device)
		for _, item := range usable {
			resolved := resolve(item, services)
			key := resolved.Serial
			if resolved.Host != "" {
				key = resolved.Host + ":" + resolved.Port
			}
			resolved.Serial = item.Serial
			unique[key] = resolved
		}
		if len(unique) == 1 {
			for _, item := range unique {
				return &item, nil
			}
		}
	}
	return nil, errors.New("连接了多台手机，无法确定投屏或推送目标；请直接选择设备或断开其他手机")
}

func resolve(value device, services []device) device {
	for _, item := range services {
		if (item.Serial != "" && item.Serial == value.Serial) || (value.Host != "" && item.Host == value.Host && item.Port == value.Port) {
			if item.Serial == "" {
				item.Serial = value.Serial
			}
			return item
		}
	}
	return value
}

func scanProgress(reader io.Reader, report func(int, string)) string {
	buffer := make([]byte, 4096)
	pending := []byte{}
	summary := ""
	last := -1
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			pending = append(pending, buffer[:count]...)
			for {
				index := bytes.IndexAny(pending, "\r\n")
				if index < 0 {
					break
				}
				line := pending[:index]
				pending = pending[index+1:]
				if match := percentPattern.FindSubmatch(line); match != nil {
					if value, convErr := strconv.Atoi(string(match[1])); convErr == nil {
						if value > 100 {
							value = 100
						}
						if value != last {
							last = value
							target := cleanLine(ansiPattern.ReplaceAllString(string(line), ""))
							if index := strings.Index(target, "]"); index >= 0 {
								target = strings.TrimSpace(target[index+1:])
							}
							report(value, target)
						}
					}
				} else if text := cleanLine(string(line)); text != "" {
					summary = text
				}
			}
			if len(pending) > 8192 {
				pending = pending[len(pending)-4096:]
			}
		}
		if err != nil {
			break
		}
	}
	if text := cleanLine(string(pending)); text != "" && !percentPattern.MatchString(text) {
		summary = text
	}
	return summary
}

func runTransfer(serial string, locals []string, remoteDir string, report func(int, string)) (string, error) {
	args := append([]string{"-s", serial, "push"}, locals...)
	cmd := exec.Command("adb", append(args, remoteDir)...)
	terminal, err := pty.Start(cmd)
	if err != nil {
		return "", errors.New("无法启动 adb push，请确认已安装 ADB")
	}
	defer terminal.Close()

	waited := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		waited <- cmd.Wait()
		close(finished)
	}()

	var canceled atomic.Bool
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)
	go func() {
		select {
		case <-stop:
			canceled.Store(true)
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-finished:
		}
	}()

	summary := scanProgress(terminal, report)
	waitErr := <-waited
	if canceled.Load() {
		return summary, errors.New("推送已取消")
	}
	if waitErr != nil {
		return summary, errors.New(first(summary, "adb push 失败"))
	}
	return summary, nil
}

func push(r request, send func(map[string]any)) error {
	paths := r.Paths
	if len(paths) == 0 && strings.TrimSpace(r.Path) != "" {
		paths = []string{r.Path}
	}
	uploads, err := localFiles(paths)
	if err != nil {
		return err
	}
	serial, err := activeSerial()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(uploads))
	locals := make([]string, 0, len(uploads))
	for _, item := range uploads {
		names = append(names, item.Name)
		locals = append(locals, item.Path)
	}
	send(map[string]any{"ok": true, "event": "start", "names": names, "count": len(uploads), "target": downloadDir})
	index, last := 0, -1
	summary, err := transfer(serial, locals, downloadDir, func(filePercent int, target string) {
		if base := filepath.Base(strings.TrimSpace(target)); base != "" && base != "." {
			for position, item := range uploads {
				if item.Name == base {
					index = position
					break
				}
			}
		}
		overall := overallPercent(uploads, index, filePercent)
		if overall != last {
			last = overall
			send(map[string]any{"ok": true, "event": "progress", "percent": overall, "filePercent": filePercent,
				"name": uploads[index].Name, "index": index + 1, "count": len(uploads)})
		}
	})
	if err != nil {
		return err
	}
	message := "已推送 " + strconv.Itoa(len(uploads)) + " 个文件到 " + downloadDir
	if len(uploads) == 1 {
		message = "已推送 " + uploads[0].Name + " 到 " + downloadDir + uploads[0].Name
	}
	if trimmed := strings.TrimSpace(summary); trimmed != "" && utf8.RuneCountInString(trimmed) <= 160 {
		message += "（" + trimmed + "）"
	}
	send(map[string]any{"ok": true, "event": "done", "names": names, "count": len(uploads),
		"target": downloadDir, "message": message})
	return nil
}

func pushMain() int {
	var guard sync.Mutex
	send := func(value map[string]any) {
		guard.Lock()
		defer guard.Unlock()
		data, err := json.Marshal(value)
		if err != nil {
			return
		}
		fmt.Println(string(data))
	}
	r := request{}
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&r); err != nil {
		send(map[string]any{"ok": false, "final": true, "message": "查询请求格式无效。"})
		return 1
	}
	if err := push(r, send); err != nil {
		send(map[string]any{"ok": false, "final": true, "message": err.Error()})
		return 1
	}
	return 0
}

func connect(r request) (map[string]any, error) {
	host, port := strings.TrimSpace(r.Host), strings.TrimSpace(r.Port)
	if port == "" {
		var found []device
		for _, item := range browse("_adb-tls-connect._tcp") {
			if item.Host == host {
				found = append(found, item)
			}
		}
		if len(found) != 1 {
			return nil, errors.New("未发现唯一的连接端口，请填写手机无线调试主页显示的连接端口")
		}
		port = found[0].Port
	}
	serial, err := address(host, port)
	if err != nil {
		return nil, err
	}
	res := execute(12*time.Second, "", "adb", "connect", serial)
	serials, checkErr := devices()
	if res.Code != 0 || checkErr != nil || !slices.Contains(serials, serial) {
		return nil, errors.New(first(res.Err, res.Text, "手机未连接，请检查无线调试与同一局域网"))
	}
	return show(device{Host: host, Port: port, Serial: serial}, r.Options)
}

func show(value device, opt options) (map[string]any, error) {
	path, err := start(value.Serial, opt)
	if err != nil {
		return nil, err
	}
	// 先启动已有连接的投屏，再用扫描结果补充地址，避免扫描失败阻断投屏。
	if value.Host != "" || strings.HasSuffix(value.Serial, "._adb-tls-connect._tcp") {
		value = resolve(value, browse("_adb-tls-connect._tcp"))
	}
	if err := save(value); err != nil {
		return nil, err
	}
	message := "已启动 scrcpy 投屏"
	if path != "" {
		message += "，录制到 " + path
	}
	return map[string]any{"message": message, "host": value.Host, "port": value.Port, "saved": value}, nil
}

func pair(r request) (map[string]any, error) {
	if ok, _ := regexp.MatchString(`^[0-9]{6}$`, strings.TrimSpace(r.Code)); !ok {
		return nil, errors.New("请输入手机显示的 6 位配对码")
	}
	host, port := strings.TrimSpace(r.Host), strings.TrimSpace(r.Port)
	if host == "" && port == "" {
		found := browse("_adb-tls-pairing._tcp")
		if len(found) != 1 {
			return nil, errors.New("未找到唯一的配对设备；请打开手机配对码页面并扫描，或填写配对地址和端口")
		}
		host, port = found[0].Host, found[0].Port
	} else if port == "" {
		var found []device
		for _, item := range browse("_adb-tls-pairing._tcp") {
			if item.Host == host {
				found = append(found, item)
			}
		}
		if len(found) != 1 {
			return nil, errors.New("未发现唯一的配对端口，请填写手机配对码页面显示的配对端口")
		}
		port = found[0].Port
	}
	serial, err := address(host, port)
	if err != nil {
		return nil, err
	}
	res := execute(20*time.Second, r.Code+"\n", "adb", "pair", serial)
	if res.Code != 0 || !strings.Contains(res.Text, "Successfully paired") {
		return nil, errors.New(first(res.Err, res.Text, "配对失败，请检查地址和配对码"))
	}
	connectPort := ""
	for range 3 {
		var found []device
		for _, item := range browse("_adb-tls-connect._tcp") {
			if item.Host == host {
				found = append(found, item)
			}
		}
		if len(found) == 1 {
			connectPort = found[0].Port
			break
		}
		pause(time.Second)
	}
	if connectPort == "" {
		return map[string]any{"message": "配对成功。未发现连接端口，请填写手机无线调试主页的地址和端口后连接。",
			"host": host, "port": "", "paired": true}, nil
	}
	r.Host, r.Port = host, connectPort
	value, err := connect(r)
	if err != nil {
		return map[string]any{"message": "配对成功，但连接失败：" + err.Error() + "。可在无线调试主页查看连接端口后重试。",
			"host": host, "port": connectPort, "paired": true}, nil
	}
	value["paired"] = true
	return value, nil
}

func handle(action string, r request) (map[string]any, error) {
	switch action {
	case "status":
		value, err := connected()
		return map[string]any{"connected": value}, err
	case "state":
		value := known()
		if value == nil {
			value = read("device")
		}
		return map[string]any{"saved": value}, nil
	case "scan":
		return map[string]any{"pairing": browse("_adb-tls-pairing._tcp"), "connections": browse("_adb-tls-connect._tcp"), "saved": read("device")}, nil
	case "pair":
		return pair(r)
	case "connect":
		return connect(r)
	case "reconnect":
		active, err := activeDevice()
		if err != nil {
			return nil, err
		}
		if active != nil {
			return show(*active, r.Options)
		}
		value := read("device")
		if value == nil {
			return nil, errors.New("尚未记住设备")
		}
		var found []device
		var matching []device
		for _, item := range browse("_adb-tls-connect._tcp") {
			if value.Serial != "" && item.Serial == value.Serial {
				matching = append(matching, item)
			}
			if value.Host != "" && item.Host == value.Host {
				found = append(found, item)
			}
		}
		if len(matching) > 0 {
			found = matching
		}
		if len(found) > 1 {
			return nil, errors.New("发现多个连接地址，请扫描并选择手机当前的连接地址")
		}
		if len(found) == 1 {
			value = &found[0]
		}
		if value.Host == "" || value.Port == "" {
			return nil, errors.New("手机当前未连接，请开启无线调试并扫描，或填写当前连接地址")
		}
		r.Host, r.Port = value.Host, value.Port
		return connect(r)
	case "forget":
		if value := read("device"); value != nil {
			if err := write("ignored", *value); err != nil {
				return nil, err
			}
		}
		if err := os.Remove(file("device")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return map[string]any{"saved": nil, "message": "已忘记设备"}, nil
	default:
		return nil, errors.New("未知操作")
	}
}

func main() {
	response := map[string]any{}
	action := ""
	if len(os.Args) > 1 {
		action = os.Args[1]
	}
	if action == "push" {
		os.Exit(pushMain())
	}
	r := request{}
	var err error
	if action != "status" {
		decoder := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20))
		err = decoder.Decode(&r)
		if err != nil {
			err = errors.New("查询请求格式无效。")
		}
	}
	if err == nil {
		response, err = handle(action, r)
	}
	if err != nil {
		response = map[string]any{"ok": false, "message": err.Error()}
	} else {
		response["ok"] = true
	}
	data, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		data = []byte(`{"ok":false,"message":"结果编码失败"}`)
	}
	fmt.Println(string(data))
}
