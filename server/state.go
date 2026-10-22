package server

import (
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/b1naryth1ef/jambon/tacview"
)

type StateObject struct {
	Id         uint64            `json:"id"`
	Types      []string          `json:"types"`
	Properties map[string]string `json:"properties"`
	Latitude   float64           `json:"latitude"`
	Longitude  float64           `json:"longitude"`
	Altitude   float64           `json:"altitude"`
	Heading    float64           `json:"heading"`
	UpdatedAt  int64             `json:"updated_at"`
	CreatedAt  int64             `json:"created_at"`

	Deleted bool `json:"-"`
}

func NewStateObject(ts int64, sourceObj *tacview.Object, coordBase [2]float64) (*StateObject, error) {
	obj := &StateObject{
		Id:         sourceObj.Id,
		Types:      []string{},
		Properties: make(map[string]string),
		Deleted:    false,
		CreatedAt:  ts,
	}

	err := obj.update(ts, sourceObj, coordBase)
	if err != nil {
		return nil, err
	}

	return obj, nil
}

func (obj *StateObject) updateLocation(data string, coordBase [2]float64) error {
	parts := strings.Split(data, "|")

	if len(parts) >= 3 {
		if parts[0] != "" {
			lng, err := strconv.ParseFloat(parts[0], 64)
			if err != nil {
				return err
			}
			obj.Longitude = lng + coordBase[1]
		}

		if parts[1] != "" {
			lat, err := strconv.ParseFloat(parts[1], 64)
			if err != nil {
				return err
			}
			obj.Latitude = lat + coordBase[0]
		}

		if parts[2] != "" {
			alt, err := strconv.ParseFloat(parts[2], 64)
			if err != nil {
				return err
			}
			obj.Altitude = alt
		}

		if len(parts) == 9 && parts[8] != "" {
			heading, err := strconv.ParseFloat(parts[8], 64)
			if err != nil {
				return err
			}
			obj.Heading = heading
		}
	}
	return nil
}

func (obj *StateObject) update(ts int64, sourceObj *tacview.Object, coordBase [2]float64) error {
	if sourceObj.Deleted {
		obj.Deleted = true
	} else {
		if obj.Deleted {
			// Tacview re-used the id of an object removed since the last
			// radar sweep; treat it as a new contact.
			obj.Deleted = false
			obj.CreatedAt = ts
		}
		for _, prop := range sourceObj.Properties {
			if prop.Key == "T" {
				err := obj.updateLocation(prop.Value, coordBase)
				if err != nil {
					return err
				}
			} else if prop.Key == "Type" {
				obj.Types = strings.Split(prop.Value, "+")
			} else {
				obj.Properties[prop.Key] = prop.Value
			}
		}
	}
	obj.UpdatedAt = ts
	return nil
}

type sessionState struct {
	sync.RWMutex

	// Session ID (the tacview recording time)
	sessionId string

	// Base to use for all incoming coordinates
	coordBase [2]float64

	// Tracked objects
	objects map[uint64]*StateObject

	offset int64
	active bool

	// Incremented for every new Tacview connection
	generation uint64
}

// Called when our connection is interrupted. Returns the ids of the objects
// that were being tracked.
func (s *sessionState) reset() []uint64 {
	s.Lock()
	defer s.Unlock()
	return s.resetLocked()
}

func (s *sessionState) resetLocked() []uint64 {
	removed := make([]uint64, 0, len(s.objects))
	for id := range s.objects {
		removed = append(removed, id)
	}
	s.objects = make(map[uint64]*StateObject)
	s.active = false
	s.offset = 0
	return removed
}

// Called when the tacview stream starts. Returns a snapshot of the objects
// in the initial time frame and the session id.
func (s *sessionState) initialize(header *tacview.Header) ([]*StateObject, string, error) {
	s.Lock()
	defer s.Unlock()
	s.resetLocked()

	globalObj := header.InitialTimeFrame.Get(0)
	if globalObj == nil {
		return nil, "", errors.New("TacView initial time frame is missing global object")
	}

	s.sessionId = ""
	if sessionId := globalObj.Get("RecordingTime"); sessionId != nil {
		s.sessionId = sessionId.Value
	}

	refLat := globalObj.Get("ReferenceLatitude")
	refLng := globalObj.Get("ReferenceLongitude")

	s.coordBase = [2]float64{0.0, 0.0}
	if refLat != nil && refLng != nil {
		refLatF, err := strconv.ParseFloat(refLat.Value, 64)
		if err != nil {
			return nil, "", err
		}
		refLngF, err := strconv.ParseFloat(refLng.Value, 64)
		if err != nil {
			return nil, "", err
		}

		s.coordBase = [2]float64{refLatF, refLngF}
	}

	s.generation++
	s.active = true
	s.update(&header.InitialTimeFrame)

	objects := make([]*StateObject, 0, len(s.objects))
	for _, object := range s.objects {
		objects = append(objects, object)
	}
	return objects, s.sessionId, nil
}

func (s *sessionState) update(tf *tacview.TimeFrame) {
	s.offset = int64(tf.Offset)
	for _, object := range tf.Objects {
		if _, exists := s.objects[object.Id]; exists {
			s.objects[object.Id].update(int64(tf.Offset), object, s.coordBase)
		} else if !object.Deleted {
			stateObj, err := NewStateObject(int64(tf.Offset), object, s.coordBase)
			if err != nil {
				log.Printf("Error processing object: %v", err)
				continue
			}

			s.objects[object.Id] = stateObj
		}
	}
}
