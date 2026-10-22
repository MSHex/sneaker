package server

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

type sessionRadarSnapshotData struct {
	Offset  int64          `json:"offset"`
	Created []*StateObject `json:"created"`
	Updated []*StateObject `json:"updated"`
	Deleted []uint64       `json:"deleted"`
}

type sessionStateData struct {
	SessionId string         `json:"session_id"`
	Offset    int64          `json:"offset"`
	Objects   []*StateObject `json:"objects"`
}

type serverSession struct {
	sync.Mutex

	server *TacViewServerConfig

	subscriberIdx int
	subscribers   map[int]chan<- []byte
	state         sessionState

	// Tacview connection status, guarded by the session mutex
	tacviewConnected bool
	tacviewError     string
}

type TacViewStatus struct {
	Connected bool   `json:"connected"`
	Target    string `json:"target"`
	Error     string `json:"error,omitempty"`
}

func (s *serverSession) GetTacViewStatus() TacViewStatus {
	s.Lock()
	defer s.Unlock()
	port := s.server.Port
	if port == 0 {
		port = defaultTacViewPort
	}
	return TacViewStatus{
		Connected: s.tacviewConnected,
		Target:    fmt.Sprintf("%v:%v", normalizeDialHost(s.server.Hostname), port),
		Error:     s.tacviewError,
	}
}

func (s *serverSession) setTacViewStatus(connected bool, errMsg string) {
	s.Lock()
	s.tacviewConnected = connected
	s.tacviewError = errMsg
	s.Unlock()
}

func newServerSession(server *TacViewServerConfig) (*serverSession, error) {
	return &serverSession{server: server, subscribers: make(map[int]chan<- []byte)}, nil
}

type PlayerMetadata struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func (s *serverSession) GetPlayerList() []PlayerMetadata {
	players := []PlayerMetadata{}
	s.state.RLock()
	defer s.state.RUnlock()

	for _, object := range s.state.objects {
		if object.Deleted {
			continue
		}

		isAir := false
		for _, typeName := range object.Types {
			if typeName == "Air" {
				isAir = true
				break
			}
		}
		if !isAir {
			continue
		}

		pilotName, ok := object.Properties["Pilot"]
		if !ok || pilotName == "" {
			continue
		}

		// AI pilots are named after their group
		if group := object.Properties["Group"]; group != "" && strings.HasPrefix(pilotName, group) {
			continue
		}

		players = append(players, PlayerMetadata{
			Name: pilotName,
			Type: object.Properties["Name"],
		})
	}
	return players
}

func encodeEvent(event string, data interface{}) ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"e": event,
		"d": data,
	})
}

func (s *serverSession) updateLoop() {
	refreshRate := time.Duration(5)
	if s.server.RadarRefreshRate > 0 {
		refreshRate = time.Duration(s.server.RadarRefreshRate)
	}
	ticker := time.NewTicker(time.Second * refreshRate)
	defer ticker.Stop()

	var currentOffset int64
	var currentGeneration uint64
	for range ticker.C {
		s.state.Lock()
		if !s.state.active {
			s.state.Unlock()
			continue
		}

		// A new Tacview connection (mission restart / server restart) starts
		// the offsets again at zero. Without this reset nothing would be sent
		// as created/updated until the new mission time passed the old one,
		// which looked like a frozen radar after every mission change.
		if s.state.generation != currentGeneration {
			currentGeneration = s.state.generation
			currentOffset = -1
		}

		data := &sessionRadarSnapshotData{
			Offset:  s.state.offset,
			Created: make([]*StateObject, 0),
			Updated: make([]*StateObject, 0),
			Deleted: make([]uint64, 0),
		}

		for id, object := range s.state.objects {
			if object.Deleted {
				data.Deleted = append(data.Deleted, id)
				delete(s.state.objects, id)
			} else if object.CreatedAt > currentOffset {
				data.Created = append(data.Created, object)
			} else if object.UpdatedAt > currentOffset {
				data.Updated = append(data.Updated, object)
			}
		}

		currentOffset = s.state.offset

		// Encode while still holding the lock: the objects are mutated by the
		// Tacview reader goroutine, and marshalling their property maps
		// concurrently is a fatal "concurrent map read and map write" crash.
		encoded, err := encodeEvent("SESSION_RADAR_SNAPSHOT", data)
		s.state.Unlock()

		if err != nil {
			log.Printf("[session:%v] failed to encode radar snapshot: %v", s.server.Name, err)
			continue
		}
		s.publishEncoded(encoded)
	}
}

// getInitialState returns the pre-encoded SESSION_STATE and
// SESSION_RADAR_SNAPSHOT events for a new subscriber, or nil if no Tacview
// session is active.
func (s *serverSession) getInitialState() [][]byte {
	s.state.RLock()
	defer s.state.RUnlock()

	if !s.state.active {
		return nil
	}

	objects := make([]*StateObject, 0, len(s.state.objects))
	for _, object := range s.state.objects {
		if !object.Deleted {
			objects = append(objects, object)
		}
	}

	stateEvent, err := encodeEvent("SESSION_STATE", &sessionStateData{
		SessionId: s.state.sessionId,
		Offset:    s.state.offset,
	})
	if err != nil {
		return nil
	}
	snapshotEvent, err := encodeEvent("SESSION_RADAR_SNAPSHOT", &sessionRadarSnapshotData{
		Offset:  s.state.offset,
		Created: objects,
		Updated: []*StateObject{},
		Deleted: []uint64{},
	})
	if err != nil {
		return nil
	}
	return [][]byte{stateEvent, snapshotEvent}
}

func (s *serverSession) publishEncoded(encoded []byte) {
	s.Lock()
	defer s.Unlock()
	for id, sub := range s.subscribers {
		select {
		case sub <- encoded:
		default:
			log.Printf("[session:%v] subscriber %v non-responsive, closing", s.server.Name, id)
			delete(s.subscribers, id)
			close(sub)
		}
	}
}

func (s *serverSession) run() {
	go s.updateLoop()

	backoff := 5 * time.Second
	lastError := ""
	for {
		started := time.Now()
		err := s.runTacViewClient()

		// Connection is gone: clear the stale picture so clients do not keep
		// showing frozen tracks from a mission that has ended.
		if removed := s.state.reset(); len(removed) > 0 {
			// Remove the tracks but keep the global object (id 0), so the map
			// and any drawings stay up while DCS loads the next mission.
			deleted := make([]uint64, 0, len(removed))
			for _, id := range removed {
				if id != 0 {
					deleted = append(deleted, id)
				}
			}
			if encoded, encErr := encodeEvent("SESSION_RADAR_SNAPSHOT", &sessionRadarSnapshotData{
				Created: []*StateObject{},
				Updated: []*StateObject{},
				Deleted: deleted,
			}); encErr == nil {
				s.publishEncoded(encoded)
			}
		}

		if err != nil {
			s.setTacViewStatus(false, err.Error())
		} else {
			s.setTacViewStatus(false, "Tacview stream ended, reconnecting")
		}

		if err != nil {
			// DCS being down between missions is normal; only log when the
			// reason changes so the DCSServerBot console is not flooded.
			if msg := err.Error(); msg != lastError {
				log.Printf("[session:%v] tacview connection to %v:%v unavailable: %v",
					s.server.Name, normalizeDialHost(s.server.Hostname), s.server.Port, err)
				lastError = msg
			}
		} else {
			lastError = ""
		}

		if time.Since(started) > time.Minute {
			backoff = 5 * time.Second
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff += 5 * time.Second
		}
	}
}

func (s *serverSession) runTacViewClient() error {
	client := NewTacViewClient(s.server.Hostname, s.server.Port, s.server.Password, "sneaker")
	header, timeFrameStream, closeConn, streamErr, err := client.Start()
	if err != nil {
		return err
	}
	defer closeConn()

	objects, sessionId, err := s.state.initialize(header)
	if err != nil {
		return err
	}

	s.setTacViewStatus(true, "")
	log.Printf("[session:%v] tacview client session initialized (%v objects)", s.server.Name, len(objects))

	s.state.RLock()
	encoded, err := encodeEvent("SESSION_STATE", &sessionStateData{
		SessionId: sessionId,
		Objects:   objects,
	})
	s.state.RUnlock()
	if err == nil {
		s.publishEncoded(encoded)
	}

	for timeFrame := range timeFrameStream {
		s.state.Lock()
		s.state.update(timeFrame)
		s.state.Unlock()
	}

	log.Printf("[session:%v] tacview stream ended: %v", s.server.Name, *streamErr)
	return nil
}

func (s *serverSession) removeSub(id int) {
	s.Lock()
	defer s.Unlock()
	delete(s.subscribers, id)
}

func (s *serverSession) addSub() (<-chan []byte, func()) {
	sub := make(chan []byte, 64)
	s.Lock()
	id := s.subscriberIdx
	s.subscribers[id] = sub
	s.subscriberIdx += 1
	s.Unlock()
	return sub, func() {
		s.removeSub(id)
	}
}
