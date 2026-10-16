package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/mdns"
)

func setup(t *testing.T) {
	t.Helper()
	oldDir, oldBrowse, oldExecute, oldStart, oldTransfer, oldPause := stateDir, browse, execute, start, transfer, pause
	stateDir = t.TempDir()
	t.Cleanup(func() {
		stateDir, browse, execute, start, transfer, pause = oldDir, oldBrowse, oldExecute, oldStart, oldTransfer, oldPause
	})
}

func TestAddress(t *testing.T) {
	if value, err := address("192.168.1.4", "37563"); err != nil || value != "192.168.1.4:37563" {
		t.Fatalf("address = %q, %v", value, err)
	}
	for _, input := range []struct{ host, port string }{
		{"192.168.1.999", "42"}, {"-s", "42"}, {"192.168.1.3", "0"}, {"example.com;whoami", "5555"},
	} {
		if _, err := address(input.host, input.port); err == nil {
			t.Errorf("accepted %q:%q", input.host, input.port)
		}
	}
}

func TestServiceDevice(t *testing.T) {
	service := "_adb-tls-connect._tcp"
	item, valid := serviceDevice(service, &mdns.ServiceEntry{
		Name: "adb-123._adb-tls-connect._tcp.local.", AddrV4: net.ParseIP("192.168.1.5"),
		Port: 41234, InfoFields: []string{"name=My Phone"},
	})
	if !valid || item.Serial != "adb-123._adb-tls-connect._tcp" || item.Name != "My Phone" {
		t.Fatalf("service = %#v, valid = %t", item, valid)
	}
	if _, valid = serviceDevice(service, &mdns.ServiceEntry{
		Name: "Charlies-Laptop._companion-link._tcp.local.", AddrV4: net.ParseIP("192.168.1.6"), Port: 63500,
	}); valid {
		t.Fatal("accepted unrelated mDNS service")
	}
}

func TestCommand(t *testing.T) {
	args, path, err := command("192.168.1.5:41234", options{}, "")
	if err != nil || path != "" || !reflect.DeepEqual(args, []string{"scrcpy", "-s", "192.168.1.5:41234"}) {
		t.Fatalf("default command = %v, %q, %v", args, path, err)
	}
	args, _, err = command("serial", options{
		ScreenOff: true, StayAwake: true, KeepActive: true, NoPowerOn: true,
		KeyboardUHID: true, Audio: "duplicate",
	}, "")
	want := []string{"scrcpy", "-s", "serial", "-S", "-w", "--keep-active", "--no-power-on", "-K", "--audio-source=playback", "--audio-dup"}
	if err != nil || !reflect.DeepEqual(args, want) {
		t.Fatalf("options command = %v, %v", args, err)
	}
	args, _, err = command("serial", options{Audio: "none"}, "")
	if err != nil || !reflect.DeepEqual(args, []string{"scrcpy", "-s", "serial", "--no-audio"}) {
		t.Fatalf("silent command = %v, %v", args, err)
	}
}

func TestRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "phone.mp4"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	args, path, err := command("serial", options{Record: true, RecordName: "phone.mp4", TimeLimit: "600"}, dir)
	if err != nil || path != filepath.Join(dir, "phone-2.mp4") || !reflect.DeepEqual(args[len(args)-2:], []string{"--record=" + path, "--time-limit=600"}) {
		t.Fatalf("record command = %v, %q, %v", args, path, err)
	}
	for _, name := range []string{"../phone.mp4", "/tmp/phone.mp4", "phone.avi", "phone\\record.mp4"} {
		if _, _, err := command("serial", options{Record: true, RecordName: name}, dir); err == nil {
			t.Errorf("accepted unsafe name %q", name)
		}
	}
	if _, _, err := command("serial", options{Record: true, TimeLimit: "-1"}, dir); err == nil {
		t.Fatal("accepted invalid time limit")
	}
}

func TestStateAndForget(t *testing.T) {
	setup(t)
	browse = func(service string) []device {
		return []device{{Host: "192.168.1.5", Port: "41234", Serial: "adb-123._adb-tls-connect._tcp"}}
	}
	execute = func(_ time.Duration, _ string, args ...string) result {
		return result{Text: "192.168.1.5:41234\tdevice\nadb-123._adb-tls-connect._tcp\tdevice\n"}
	}
	response, err := handle("state", request{})
	if err != nil || response["saved"].(*device).Host != "192.168.1.5" {
		t.Fatalf("state = %v, %v", response, err)
	}
	if info, err := os.Stat(file("device")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions = %v, %v", info, err)
	}
	if _, err := handle("forget", request{}); err != nil {
		t.Fatal(err)
	}
	response, _ = handle("state", request{})
	if response["saved"].(*device) != nil {
		t.Fatalf("forgotten device was reimported: %v", response)
	}
}

func TestSeveralPhonesAreNotSaved(t *testing.T) {
	setup(t)
	execute = func(_ time.Duration, _ string, args ...string) result {
		return result{Text: "192.168.1.5:41234\tdevice\n192.168.1.6:41235\tdevice\n"}
	}
	if saved := known(); saved != nil {
		t.Fatalf("ambiguous device saved: %#v", saved)
	}
}

func TestConnectAndPair(t *testing.T) {
	setup(t)
	var calls []string
	browse = func(service string) []device {
		if service == "_adb-tls-pairing._tcp" {
			return []device{{Host: "192.168.1.5", Port: "32100"}}
		}
		return []device{{Host: "192.168.1.5", Port: "41234"}}
	}
	execute = func(_ time.Duration, input string, args ...string) result {
		calls = append(calls, strings.Join(args, " ")+"|"+input)
		switch args[1] {
		case "pair":
			return result{Text: "Successfully paired to 192.168.1.5:32100"}
		case "devices":
			return result{Text: "192.168.1.5:41234\tdevice\n"}
		default:
			return result{Text: "connected to 192.168.1.5:41234"}
		}
	}
	start = func(serial string, _ options) (string, error) {
		if serial != "192.168.1.5:41234" {
			t.Errorf("started wrong device: %s", serial)
		}
		return "", nil
	}
	response, err := handle("pair", request{Code: "123456"})
	if err != nil || response["paired"] != true || response["port"] != "41234" {
		t.Fatalf("pair = %v, %v", response, err)
	}
	if calls[0] != "adb pair 192.168.1.5:32100|123456\n" || calls[1] != "adb connect 192.168.1.5:41234|" {
		t.Fatalf("adb call order or code transport: %#v", calls)
	}
	content, _ := os.ReadFile(file("device"))
	if strings.Contains(string(content), "123456") {
		t.Fatal("pairing code was persisted")
	}
	var saved device
	if err := json.Unmarshal(content, &saved); err != nil || saved.Port != "41234" {
		t.Fatalf("saved state = %#v, %v", saved, err)
	}
}

func TestFailedConnectKeepsSaved(t *testing.T) {
	setup(t)
	if err := save(device{Host: "192.168.1.5", Port: "41234"}); err != nil {
		t.Fatal(err)
	}
	execute = func(_ time.Duration, _ string, args ...string) result {
		return result{Code: 1, Err: "connection failed"}
	}
	start = func(string, options) (string, error) { return "", errors.New("should not run") }
	if _, err := connect(request{Host: "192.168.1.6", Port: "41235"}); err == nil {
		t.Fatal("failed connect succeeded")
	}
	if got := read("device"); got == nil || got.Host != "192.168.1.5" {
		t.Fatalf("saved device replaced on failure: %#v", got)
	}
}

func TestStatusUsesActiveADBDevice(t *testing.T) {
	setup(t)
	for _, sample := range []struct {
		listing string
		want    bool
	}{
		{"192.168.1.5:41234\tdevice\n", true},
		{"192.168.1.5:41234\toffline\n", false},
		{"192.168.1.5:41234\tunauthorized\n", false},
		{"emulator-5554\tdevice\n", false},
		{"List of devices attached\n", false},
	} {
		execute = func(_ time.Duration, _ string, _ ...string) result { return result{Text: sample.listing} }
		value, err := handle("status", request{})
		if err != nil || value["connected"] != sample.want {
			t.Errorf("status %q = %v, %v", sample.listing, value, err)
		}
	}
	execute = func(_ time.Duration, _ string, _ ...string) result { return result{Code: 1, Err: "ADB unavailable"} }
	if _, err := handle("status", request{}); err == nil || err.Error() != "ADB unavailable" {
		t.Errorf("lost ADB failure: %v", err)
	}
}

func TestMissingPortsDoNotChangeState(t *testing.T) {
	setup(t)
	if err := save(device{Host: "192.168.1.5", Port: "41234"}); err != nil {
		t.Fatal(err)
	}
	browse = func(string) []device { return nil }
	if _, err := connect(request{Host: "192.168.1.6"}); err == nil || !strings.Contains(err.Error(), "连接端口") {
		t.Errorf("missing connect port = %v", err)
	}
	if _, err := pair(request{Host: "192.168.1.6", Code: "123456"}); err == nil || !strings.Contains(err.Error(), "配对端口") {
		t.Errorf("missing pairing port = %v", err)
	}
	if got := read("device"); got == nil || got.Host != "192.168.1.5" {
		t.Fatalf("saved phone changed: %v", got)
	}
}

func TestPairWithoutConnectService(t *testing.T) {
	setup(t)
	browse = func(string) []device { return nil }
	execute = func(_ time.Duration, _ string, args ...string) result {
		if args[1] == "pair" {
			return result{Text: "Successfully paired to 192.168.1.5:32100"}
		}
		return result{Code: 1}
	}
	pause = func(time.Duration) {}
	value, err := pair(request{Host: "192.168.1.5", Port: "32100", Code: "123456"})
	if err != nil || value["paired"] != true || value["port"] != "" || read("device") != nil {
		t.Errorf("successful pairing without connect service = %v, %v", value, err)
	}
}

func TestReconnectUsesNewPortAndOptions(t *testing.T) {
	setup(t)
	if err := save(device{Host: "192.168.1.5", Port: "41234"}); err != nil {
		t.Fatal(err)
	}
	browse = func(string) []device { return []device{{Host: "192.168.1.5", Port: "49999"}} }
	connected := false
	execute = func(_ time.Duration, _ string, args ...string) result {
		if args[1] == "devices" {
			if !connected {
				return result{Text: "List of devices attached\n"}
			}
			return result{Text: "192.168.1.5:49999\tdevice\n"}
		}
		if args[2] != "192.168.1.5:49999" {
			t.Errorf("reconnect used stale port: %v", args)
		}
		connected = true
		return result{Text: "connected"}
	}
	start = func(serial string, opt options) (string, error) {
		if serial != "192.168.1.5:49999" || !opt.KeyboardUHID {
			t.Errorf("lost target or settings: %q, %v", serial, opt)
		}
		return "", nil
	}
	value, err := handle("reconnect", request{Options: options{KeyboardUHID: true}})
	if err != nil || value["port"] != "49999" || read("device").Port != "49999" {
		t.Errorf("reconnect = %v, %v", value, err)
	}
}

func TestReconnectUsesActivePhone(t *testing.T) {
	const wireless = "adb-phone._adb-tls-connect._tcp"
	for _, sample := range []struct {
		name    string
		saved   *device
		listing string
		service []device
		serial  string
		host    string
		aliases bool
	}{
		{"changed address", &device{Host: "192.168.1.5", Port: "41234"}, wireless + "\tdevice\n",
			[]device{{Host: "192.168.1.6", Port: "49999", Serial: wireless}}, wireless, "192.168.1.6", false},
		{"discovery unavailable", &device{Host: "192.168.1.5", Port: "41234"}, wireless + "\tdevice\n",
			nil, wireless, "", false},
		{"no saved device", nil, wireless + "\tdevice\n", nil, wireless, "", false},
		{"USB connection", nil, "usb-phone\tdevice\nemulator-5554\tdevice\n", nil, "usb-phone", "", false},
		{"direct address", &device{Host: "192.168.1.5", Port: "41234"}, "192.168.1.6:49999\tdevice\n",
			nil, "192.168.1.6:49999", "192.168.1.6", false},
		{"saved identity among several phones", &device{Host: "192.168.1.5", Port: "41234", Serial: wireless},
			"other-phone\tdevice\n" + wireless + "\tdevice\n", nil, wireless, "", false},
		{"two aliases of the same phone", &device{Host: "192.168.1.5", Port: "41234"},
			"192.168.1.6:49999\tdevice\n" + wireless + "\tdevice\n",
			[]device{{Host: "192.168.1.6", Port: "49999", Serial: wireless}}, wireless, "192.168.1.6", true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			setup(t)
			if sample.saved != nil {
				if err := save(*sample.saved); err != nil {
					t.Fatal(err)
				}
			}
			started := false
			execute = func(_ time.Duration, _ string, args ...string) result {
				if !reflect.DeepEqual(args, []string{"adb", "devices"}) {
					t.Errorf("connected phone triggered another ADB operation: %v", args)
					return result{Code: 1}
				}
				return result{Text: sample.listing}
			}
			browse = func(string) []device {
				if !started && !sample.aliases {
					t.Error("discovery blocked launching the connected phone")
				}
				return sample.service
			}
			start = func(serial string, opt options) (string, error) {
				started = true
				if serial != sample.serial || !opt.KeyboardUHID {
					t.Errorf("lost active phone or options: %q, %#v", serial, opt)
				}
				return "", nil
			}
			response, err := handle("reconnect", request{Options: options{KeyboardUHID: true}})
			if err != nil || !started {
				t.Fatalf("reconnect = %v, %v, started = %t", response, err, started)
			}
			if saved := read("device"); saved == nil || saved.Serial != sample.serial || saved.Host != sample.host {
				t.Fatalf("active phone was not remembered: %#v", saved)
			}
		})
	}
}

func TestReconnectRejectsAmbiguousActivePhones(t *testing.T) {
	for _, sample := range []struct {
		name    string
		listing string
		service []device
	}{
		{"several USB phones", "first-phone\tdevice\nsecond-phone\tdevice\n", nil},
		{"unconfirmed alias", "192.168.1.6:49999\tdevice\nadb-phone._adb-tls-connect._tcp\tdevice\n", nil},
		{"different wireless phones", "192.168.1.6:49999\tdevice\nadb-phone._adb-tls-connect._tcp\tdevice\n",
			[]device{{Host: "192.168.1.7", Port: "40000", Serial: "adb-phone._adb-tls-connect._tcp"}}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			setup(t)
			if err := save(device{Host: "192.168.1.5", Port: "41234"}); err != nil {
				t.Fatal(err)
			}
			execute = func(_ time.Duration, _ string, args ...string) result {
				if !reflect.DeepEqual(args, []string{"adb", "devices"}) {
					t.Errorf("ambiguous target triggered an ADB operation: %v", args)
				}
				return result{Text: sample.listing}
			}
			browse = func(string) []device { return sample.service }
			start = func(string, options) (string, error) { t.Error("launched an ambiguous phone"); return "", nil }
			if _, err := handle("reconnect", request{}); err == nil || !strings.Contains(err.Error(), "多台") {
				t.Fatalf("ambiguous phones = %v", err)
			}
			if saved := read("device"); saved == nil || saved.Host != "192.168.1.5" {
				t.Fatalf("changed saved phone on failure: %#v", saved)
			}
		})
	}
}

func TestReconnectFindsSavedIdentityAtNewAddress(t *testing.T) {
	setup(t)
	const serial = "adb-phone._adb-tls-connect._tcp"
	if err := save(device{Host: "192.168.1.5", Port: "41234", Serial: serial}); err != nil {
		t.Fatal(err)
	}
	connected := false
	browse = func(string) []device {
		return []device{{Host: "192.168.1.6", Port: "49999", Serial: serial}}
	}
	execute = func(_ time.Duration, _ string, args ...string) result {
		if args[1] == "devices" {
			if connected {
				return result{Text: "192.168.1.6:49999\tdevice\n"}
			}
			return result{Text: "List of devices attached\n"}
		}
		if !reflect.DeepEqual(args, []string{"adb", "connect", "192.168.1.6:49999"}) {
			t.Errorf("reconnected stale address: %v", args)
			return result{Code: 1}
		}
		connected = true
		return result{Text: "connected"}
	}
	start = func(target string, _ options) (string, error) {
		if target != "192.168.1.6:49999" {
			t.Errorf("wrong target: %q", target)
		}
		return "", nil
	}
	if _, err := handle("reconnect", request{}); err != nil {
		t.Fatal(err)
	}
	if saved := read("device"); saved == nil || saved.Host != "192.168.1.6" || saved.Serial != serial {
		t.Fatalf("lost identity or new address: %#v", saved)
	}
}

func TestStateUpdatesSavedAddress(t *testing.T) {
	setup(t)
	if err := save(device{Host: "192.168.1.5", Port: "41234"}); err != nil {
		t.Fatal(err)
	}
	const serial = "adb-phone._adb-tls-connect._tcp"
	execute = func(_ time.Duration, _ string, _ ...string) result {
		return result{Text: serial + "\tdevice\n"}
	}
	browse = func(string) []device {
		return []device{{Host: "192.168.1.6", Port: "49999", Serial: serial}}
	}
	response, err := handle("state", request{})
	if err != nil {
		t.Fatal(err)
	}
	if saved, ok := response["saved"].(*device); !ok || saved == nil || saved.Host != "192.168.1.6" || saved.Serial != serial {
		t.Fatalf("stale saved address: %#v", response)
	}
}

func TestUnrecognizedPairingIsNotSaved(t *testing.T) {
	setup(t)
	browse = func(string) []device { return nil }
	if _, err := pair(request{Code: "123456"}); err == nil || !strings.Contains(err.Error(), "唯一") {
		t.Errorf("accepted missing pairing service: %v", err)
	}
	browse = func(string) []device {
		return []device{{Host: "192.168.1.5", Port: "32100"}, {Host: "192.168.1.6", Port: "32101"}}
	}
	if _, err := pair(request{Code: "123456"}); err == nil || !strings.Contains(err.Error(), "唯一") {
		t.Errorf("accepted ambiguous pairing services: %v", err)
	}
	if read("device") != nil {
		t.Fatal("saved a device without explicit pairing")
	}
}

func TestLocalFileValidation(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(name, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if value, err := localFile("  " + name + "  "); err != nil || value != name {
		t.Fatalf("local file = %q, %v", value, err)
	}
	link := filepath.Join(dir, "link.mp4")
	if err := os.Symlink(name, link); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"", "relative.mp4", dir, link, filepath.Join(dir, "missing.mp4")} {
		if _, err := localFile(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestScanProgressParsesADBOutput(t *testing.T) {
	output := "\r[  1%] /sdcard/Download/clip.mp4\x1b[K\r[ 42%] /sdcard/Download/clip.mp4\x1b[K" +
		"\r[ 42%] /sdcard/Download/clip.mp4\x1b[K\r[ 99%] /sdcard/Download/clip.mp4\x1b[K" +
		"\r/tmp/clip.mp4: 1 file pushed, 0 skipped. 3.2 MB/s (1024 bytes in 0.3s)\x1b[K\r\n"
	seen := []int{}
	targets := []string{}
	summary := scanProgress(strings.NewReader(output), func(percent int, target string) {
		seen = append(seen, percent)
		targets = append(targets, target)
	})
	if !reflect.DeepEqual(seen, []int{1, 42, 99}) {
		t.Fatalf("progress = %v", seen)
	}
	if !reflect.DeepEqual(targets, []string{"/sdcard/Download/clip.mp4", "/sdcard/Download/clip.mp4", "/sdcard/Download/clip.mp4"}) {
		t.Fatalf("targets = %v", targets)
	}
	if !strings.Contains(summary, "1 file pushed") || strings.Contains(summary, "\x1b") {
		t.Fatalf("summary = %q", summary)
	}
	if value := scanProgress(strings.NewReader("no progress here\x1b[K\n"), func(int, string) {}); value != "no progress here" {
		t.Fatalf("plain summary = %q", value)
	}
	if value := scanProgress(strings.NewReader("[ 250%] clip\x1b[K\r"), func(int, string) {}); value != "" {
		t.Fatalf("progress-only output leaked into summary: %q", value)
	}
}

func TestPushReportsProgressAndTarget(t *testing.T) {
	setup(t)
	if err := save(device{Host: "192.168.1.5", Port: "41234"}); err != nil {
		t.Fatal(err)
	}
	execute = func(_ time.Duration, _ string, _ ...string) result {
		return result{Text: "192.168.1.5:41234\tdevice\n"}
	}
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(path, make([]byte, 1000), 0600); err != nil {
		t.Fatal(err)
	}
	var serial, target string
	var locals []string
	transfer = func(deviceSerial string, paths []string, remoteDir string, report func(int, string)) (string, error) {
		serial, locals, target = deviceSerial, paths, remoteDir
		report(5, "/sdcard/Download/clip.mp4")
		report(60, "/sdcard/Download/clip.mp4")
		return "clip.mp4: 1 file pushed", nil
	}
	events := []map[string]any{}
	if err := push(request{Path: path}, func(value map[string]any) { events = append(events, value) }); err != nil {
		t.Fatal(err)
	}
	if serial != "192.168.1.5:41234" || !reflect.DeepEqual(locals, []string{path}) || target != "/sdcard/Download/" {
		t.Fatalf("transfer args = %q, %v, %q", serial, locals, target)
	}
	if len(events) != 4 || events[0]["event"] != "start" || events[1]["percent"] != 5 ||
		events[2]["percent"] != 60 || events[3]["event"] != "done" {
		t.Fatalf("events = %#v", events)
	}
	if message, _ := events[3]["message"].(string); !strings.Contains(message, "clip.mp4") {
		t.Fatalf("done message = %q", message)
	}
}

func TestPushMultipleFilesUsesSizeWeightedProgress(t *testing.T) {
	setup(t)
	if err := save(device{Host: "192.168.1.5", Port: "41234"}); err != nil {
		t.Fatal(err)
	}
	execute = func(_ time.Duration, _ string, _ ...string) result {
		return result{Text: "192.168.1.5:41234\tdevice\n"}
	}
	dir := t.TempDir()
	small := filepath.Join(dir, "small.bin")
	large := filepath.Join(dir, "large.bin")
	if err := os.WriteFile(small, make([]byte, 1000), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(large, make([]byte, 3000), 0600); err != nil {
		t.Fatal(err)
	}
	var locals []string
	transfer = func(_ string, paths []string, _ string, report func(int, string)) (string, error) {
		locals = paths
		report(50, "/sdcard/Download/small.bin")
		report(0, "/sdcard/Download/large.bin")
		report(50, "/sdcard/Download/large.bin")
		return "2 files pushed", nil
	}
	events := []map[string]any{}
	if err := push(request{Paths: []string{small, large}}, func(value map[string]any) { events = append(events, value) }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(locals, []string{small, large}) {
		t.Fatalf("locals = %v", locals)
	}
	progress := []int{}
	for _, event := range events {
		if event["event"] == "progress" {
			progress = append(progress, event["percent"].(int))
		}
	}
	if !reflect.DeepEqual(progress, []int{12, 25, 62}) {
		t.Fatalf("weighted progress = %v", progress)
	}
	start := events[0]
	if start["count"] != 2 || !reflect.DeepEqual(start["names"], []string{"small.bin", "large.bin"}) {
		t.Fatalf("start event = %#v", start)
	}
}

func TestPushRejectsDuplicateNamesAndLimits(t *testing.T) {
	setup(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "one")
	second := filepath.Join(dir, "two")
	if err := os.MkdirAll(first, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(second, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "same.bin"), []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "same.bin"), []byte("b"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := localFiles([]string{filepath.Join(first, "same.bin"), filepath.Join(second, "same.bin")}); err == nil ||
		!strings.Contains(err.Error(), "重名") {
		t.Fatalf("duplicate names = %v", err)
	}
	if uploads, err := localFiles([]string{filepath.Join(first, "same.bin"), filepath.Join(first, "same.bin")}); err != nil || len(uploads) != 1 {
		t.Fatalf("deduped uploads = %v, %v", uploads, err)
	}
	over := make([]string, 51)
	for index := range over {
		over[index] = filepath.Join(first, "same.bin")
	}
	if _, err := localFiles(over); err == nil || !strings.Contains(err.Error(), "50") {
		t.Fatalf("too many files = %v", err)
	}
}

func TestPushReportsCancellation(t *testing.T) {
	setup(t)
	if err := save(device{Host: "192.168.1.5", Port: "41234"}); err != nil {
		t.Fatal(err)
	}
	execute = func(_ time.Duration, _ string, _ ...string) result {
		return result{Text: "192.168.1.5:41234\tdevice\n"}
	}
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	transfer = func(string, []string, string, func(int, string)) (string, error) {
		return "", errors.New("推送已取消")
	}
	if err := push(request{Path: path}, func(map[string]any) {}); err == nil || err.Error() != "推送已取消" {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestActiveSerialRequiresSinglePhone(t *testing.T) {
	setup(t)
	execute = func(_ time.Duration, _ string, _ ...string) result {
		return result{Text: "192.168.1.5:41234\tdevice\n192.168.1.6:41235\tdevice\n"}
	}
	if _, err := activeSerial(); err == nil || !strings.Contains(err.Error(), "多台") {
		t.Fatalf("multiple phones = %v", err)
	}
	if err := save(device{Host: "192.168.1.6", Port: "41235"}); err != nil {
		t.Fatal(err)
	}
	if serial, err := activeSerial(); err != nil || serial != "192.168.1.6:41235" {
		t.Fatalf("remembered phone = %q, %v", serial, err)
	}
	execute = func(_ time.Duration, _ string, _ ...string) result { return result{Text: "List of devices attached\n"} }
	if _, err := activeSerial(); err == nil || !strings.Contains(err.Error(), "没有已连接") {
		t.Fatalf("no phone = %v", err)
	}
}

func TestPopupAcceptsDroppedFilesAndSendsPaths(t *testing.T) {
	popout, err := os.ReadFile("PhonePopout.qml")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("PhoneSource.qml")
	if err != nil {
		t.Fatal(err)
	}
	popup := string(popout)
	if !strings.Contains(popup, "DropArea {") || !strings.Contains(popup, "drop.hasUrls") ||
		!strings.Contains(popup, "root.toLocalPaths(drop.urls)") {
		t.Fatal("popup lost its drag and drop entry point")
	}
	if !strings.Contains(popup, "FileDialog.OpenFiles") || !strings.Contains(popup, "selectedFiles") {
		t.Fatal("popup no longer supports picking several files")
	}
	if !strings.Contains(string(source), "JSON.stringify({paths: list})") {
		t.Fatal("source no longer forwards the selected path list")
	}
}

func TestLaunchKeepsScrcpyOutput(t *testing.T) {
	setup(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "scrcpy")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"drag and drop failed\" >&2\necho \"SDL_VIDEODRIVER=$SDL_VIDEODRIVER\"\nsleep 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := launch("serial", options{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	data, err := os.ReadFile(filepath.Join(stateDir, "scrcpy.log"))
	if err != nil || !strings.Contains(string(data), "drag and drop failed") {
		t.Fatalf("scrcpy log = %q, %v", string(data), err)
	}
	if !strings.Contains(string(data), "SDL_VIDEODRIVER=wayland") {
		t.Fatalf("scrcpy did not use Wayland: %q", data)
	}
}
