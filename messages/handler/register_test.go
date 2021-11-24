package handler

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/greenbone/eulabeia/connection"
	"github.com/greenbone/eulabeia/messages"
	"github.com/greenbone/eulabeia/messages/cmds"
)

var testData = connection.TopicData{Topic: "test", Message: toBytes(cmds.NewGet("test", "id", "", ""))}

func toBytes(c messages.Event) []byte {
	b, _ := json.Marshal(c)
	return b
}

func TestIncomingClosed(t *testing.T) {
	h := []Container{}
	publisher := []connection.Publisher{}
	in := make(chan *connection.TopicData, 1)
	out := make(chan *connection.SendResponse, 1)
	mh := NewRegister("test", h, nil, []connection.Preprocessor{}, publisher, in, out)
	close(in)
	if mh.Check() {
		t.Fatalf("Expected check to be false (due to closed in)")
	}
}
func TestOutgoingSuccess(t *testing.T) {
	h := []Container{}
	published := 0
	publisher := []connection.Publisher{
		connection.ClosurePublisher{
			Closure: func(s string, i interface{}) error {
				published = published + 1
				return nil
			}},
	}
	in := make(chan *connection.TopicData, 1)
	out := make(chan *connection.SendResponse, 1)
	out <- &connection.SendResponse{Topic: "Test"}
	mh := NewRegister("test", h, nil, []connection.Preprocessor{}, publisher, in, out)
	if !mh.Check() {
		t.Fatalf("Expected check to be true")
	}
	if published != 1 {
		t.Fatalf("Expected publisher to be called once")
	}

}
func TestOutgoingClosed(t *testing.T) {
	h := []Container{}
	publisher := []connection.Publisher{}
	in := make(chan *connection.TopicData, 1)
	out := make(chan *connection.SendResponse, 1)
	close(out)
	mh := NewRegister("test", h, nil, []connection.Preprocessor{}, publisher, in, out)
	if mh.Check() {
		t.Fatalf("Expected check to be false")
	}
}
func TestOutgoingFailureNotPanic(t *testing.T) {
	h := []Container{}
	published := 0
	publisher := []connection.Publisher{
		connection.ClosurePublisher{Closure: func(s string, i interface{}) error {
			published = published + 1
			return errors.New("Something")
		}},
	}
	in := make(chan *connection.TopicData, 1)
	out := make(chan *connection.SendResponse, 1)
	out <- &connection.SendResponse{Topic: "Test"}
	mh := NewRegister("test", h, nil, []connection.Preprocessor{}, publisher, in, out)
	if !mh.Check() {
		t.Fatalf("Expected check to be true")
	}
	if published != 1 {
		t.Fatalf("Expected publisher to be called once")
	}

}
