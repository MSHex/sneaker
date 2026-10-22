package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"
)

// fakeTacview emulates the Tacview real-time telemetry server built into the
// Tacview DCS exporter. Each accepted connection gets a handshake, a header
// and then whatever the frames callback writes.
func fakeTacview(t *testing.T, frames func(conn net.Conn, mission int)) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		mission := 0
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mission++
			go func(conn net.Conn, mission int) {
				defer conn.Close()
				conn.Write([]byte("XtraLib.Stream.0\nTacview.RealTimeTelemetry.0\nDCS-Host\n\x00"))
				r := bufio.NewReader(conn)
				for i := 0; i < 3; i++ {
					if _, err := r.ReadString('\n'); err != nil {
						return
					}
				}
				if _, err := r.ReadString('\x00'); err != nil {
					return
				}
				fmt.Fprintf(conn, "\ufeffFileType=text/acmi/tacview\r\nFileVersion=2.2\r\n")
				fmt.Fprintf(conn, "0,ReferenceTime=2026-10-22T10:00:00Z,RecordingTime=mission-%d,ReferenceLongitude=44,ReferenceLatitude=33\n", mission)
				fmt.Fprintf(conn, "0,Title=Test\\, mission\n")
				frames(conn, mission)
			}(conn, mission)
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	return "0.0.0.0", addr.Port // wildcard on purpose: must be dialed as loopback
}

func TestParseACMIObjectLine(t *testing.T) {
	obj, err := parseACMIObjectLine(`1a2,T=1|2|3,Name=F-16C_50,Pilot=Curse\, Hexe 1-1,Group=Hexe`)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Id != 0x1a2 || obj.Get("Pilot").Value != "Curse, Hexe 1-1" || obj.Get("Group").Value != "Hexe" {
		t.Fatalf("unexpected object: %+v", obj)
	}

	obj, err = parseACMIObjectLine("-1a2")
	if err != nil || !obj.Deleted || obj.Id != 0x1a2 {
		t.Fatalf("delete line: %+v %v", obj, err)
	}

	// No properties: must not panic
	if _, err := parseACMIObjectLine("42"); err != nil {
		t.Fatal(err)
	}
	if _, err := parseACMIObjectLine("zz,T=1"); err == nil {
		t.Fatal("expected error for invalid id")
	}
}

func TestSessionSurvivesBadLinesAndMissionRestart(t *testing.T) {
	host, port := fakeTacview(t, func(conn net.Conn, mission int) {
		w := bufio.NewWriter(conn)
		for i := 1; i <= 40; i++ {
			fmt.Fprintf(w, "#%d\n", i)
			fmt.Fprintf(w, "101,T=%d|0|5000|||0|0|0,Type=Air+FixedWing,Name=F-16C_50,Pilot=Curse,Group=Hexe,Coalition=Enemies\n", i)
			if i == 2 {
				w.WriteString("this is not acmi\n")
				w.WriteString("// comment line\n")
				w.WriteString("\n")
				w.WriteString("102\n")
			}
			if i == 3 {
				fmt.Fprintf(w, "103,T=1|1|100,Type=Ground+Vehicle,Name=Multi\\\nline\n")
			}
			w.Flush()
			time.Sleep(50 * time.Millisecond)
		}
		// mission ends -> connection closes
	})

	sess, _ := newServerSession(&TacViewServerConfig{Name: "test", Hostname: host, Port: port, RadarRefreshRate: 1})
	go sess.run()

	sub, closeSub := sess.addSub()
	defer closeSub()

	sawMission := map[string]bool{}
	createdAfterRestart := false
	deletedOnDisconnect := false
	missionsSeen := 0
	deadline := time.After(25 * time.Second)

	for !(createdAfterRestart && deletedOnDisconnect) {
		select {
		case msg := <-sub:
			var ev struct {
				E string          `json:"e"`
				D json.RawMessage `json:"d"`
			}
			if err := json.Unmarshal(msg, &ev); err != nil {
				t.Fatal(err)
			}
			switch ev.E {
			case "SESSION_STATE":
				var d sessionStateData
				json.Unmarshal(ev.D, &d)
				if !sawMission[d.SessionId] {
					sawMission[d.SessionId] = true
					missionsSeen++
				}
			case "SESSION_RADAR_SNAPSHOT":
				var d sessionRadarSnapshotData
				json.Unmarshal(ev.D, &d)
				for _, id := range d.Deleted {
					if id == 0x101 {
						deletedOnDisconnect = true
					}
					if id == 0 {
						t.Fatal("global object must not be deleted on disconnect")
					}
				}
				if missionsSeen >= 2 {
					for _, o := range d.Created {
						if o.Id == 0x101 {
							createdAfterRestart = true
						}
					}
				}
			}
		case <-deadline:
			t.Fatalf("timeout: missions=%v createdAfterRestart=%v deletedOnDisconnect=%v",
				sawMission, createdAfterRestart, deletedOnDisconnect)
		}
	}

	if got := sess.GetPlayerList(); len(got) > 1 {
		t.Fatalf("unexpected players: %+v", got)
	}
}

func TestNormalizeDialHost(t *testing.T) {
	for in, want := range map[string]string{
		"":            "127.0.0.1",
		"0.0.0.0":     "127.0.0.1",
		"[::]":        "127.0.0.1",
		"10.0.0.5":    "10.0.0.5",
		" host.lan ":  "host.lan",
		"192.168.1.2": "192.168.1.2",
	} {
		if got := normalizeDialHost(in); got != want {
			t.Errorf("normalizeDialHost(%q) = %q, want %q", in, got, want)
		}
	}
}
