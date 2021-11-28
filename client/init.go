package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/greenbone/eulabeia/connection"
	"github.com/greenbone/eulabeia/messages"
	"github.com/greenbone/eulabeia/messages/cmds"
	"github.com/greenbone/eulabeia/messages/info"
	"github.com/greenbone/eulabeia/models"
	"github.com/rs/zerolog/log"
)

// Verify and Parse uses init event and received bytes to return bool for finished, messages.Event
// the parsed bytes or an error
type VerifyAndParse func(messages.Event, []byte, messages.Message) (bool, messages.Event, error)
type OnPrevious func(messages.Event) messages.Event

type State int8

const (
	None    = -1
	Failure = 0
	Success = 1
)

type Received struct {
	State State
	Event messages.Event
}

type Program struct {
	sync.RWMutex
	context string
	name    string
	init    messages.Event
	// TODO combine them and return State
	verifySuccess     VerifyAndParse
	verifyFailure     VerifyAndParse
	onPreviousSuccess OnPrevious
	onPreviousFailure OnPrevious
	success           messages.Event
	failure           messages.Event
	next              *Program
	previous          *Program
	send              chan<- *connection.SendResponse
	receive           <-chan *connection.TopicData
	received          chan<- *Received // used to inform downstream about failure or success; mostly useful on multiple success or fialure states (e.g. a scan)
	timeout           time.Duration    // Timeout of trying to receive a response; a timeout is mandataroy ohterwise it could block
	finish            bool
}

func (p *Program) onMessage(td *connection.TopicData) {
	var maym messages.Message
	if err := json.Unmarshal(td.Message, &maym); err != nil {
		log.Trace().Err(err).Msgf("Skipping message (%s) on %s because it is not parseable to Message", string(td.Message), td.Topic)
		return
	}
	finish, msg, err := p.verifySuccess(p.init, td.Message, maym)
	var received *Received
	p.Lock()
	defer p.Unlock()
	if err != nil {
		log.Trace().Err(err).Msg("Unable verify sucess, trying to verify failure")
		if finish, msg, e := p.verifyFailure(p.init, td.Message, maym); e == nil {
			p.failure = msg
			p.finish = finish
			log.Trace().Msgf("Setting Failure to %T", msg)
			received = &Received{
				State: Failure,
				Event: msg,
			}
		} else {
			log.Trace().Err(e).Msgf("Ignoring message %s", maym.Type)
		}
	} else {
		log.Trace().Msgf("Setting success to %T", msg)
		p.success = msg
		p.finish = finish
		received = &Received{
			State: Success,
			Event: msg,
		}
	}

	if received != nil && p.received != nil {
		p.received <- received
	}
}

func (p *Program) Next(
	onFailure, onSuccess OnPrevious,
	verifyFailure, verifySuccess VerifyAndParse,
) *Program {
	np := &Program{
		context:           p.context,
		verifySuccess:     verifySuccess,
		verifyFailure:     verifyFailure,
		onPreviousSuccess: onSuccess,
		onPreviousFailure: onFailure,
		send:              p.send,
		receive:           p.receive,
		previous:          p,
		received:          p.received,
		timeout:           p.timeout,
	}
	p.next = np
	return np
}

// First finds the first step within a Program
func (p *Program) First() *Program {
	p.RLock()
	defer p.RUnlock()
	var result *Program = p
	for result.previous != nil {
		result = result.previous
	}
	return result
}

// Start identifies the First step and runs it
func (p *Program) Start() (success interface{}, failure interface{}, err error) {
	return p.First().Run()
}

// Run runs the current and following steps
func (p *Program) Run() (success interface{}, failure interface{}, err error) {
	// it is not a startpoint and depends on a previous program

	if p.init == nil {
		if p.previous == nil {
			return nil, nil, errors.New("unable to calculate init event without previous program")
		}
		if p.previous.success == nil && p.previous.failure == nil {
			return nil, nil, errors.New("previous program did not run yet")
		}
		if p.previous.failure != nil && p.onPreviousFailure != nil {
			p.init = p.onPreviousFailure(p.previous.failure)
		} else if p.previous.success != nil && p.onPreviousSuccess != nil {
			p.init = p.onPreviousSuccess(p.previous.success)
		}
	}
	if p.init == nil {
		return nil, nil, errors.New("no initial send found")
	}
	select {
	case p.send <- messages.EventToResponse(p.context, p.init):
	case <-time.After(p.timeout):
		return nil, nil, fmt.Errorf("timeout after %s", p.timeout)
	}
	log.Trace().Msgf("Sent %+v", p.init)
	for !p.finish {
		select {
		case td, open := <-p.receive:
			p.onMessage(td)
			if !open {
				return nil, nil, errors.New("channel in closed")
			}
		case <-time.After(p.timeout):
			return nil, nil, fmt.Errorf("timeout after %s", p.timeout)
		}
	}
	success = p.success
	failure = p.failure
	if success == nil {
		err = errors.New("program failed")
	}
	if p.next != nil {
		log.Trace().Msgf("Running next %s", p.name)
		success, failure, err = p.next.Run()
	}
	return

}

func ModifyBasedOnGetID(aggregate string, destination string, values func(messages.GetID) map[string]interface{}) func(messages.Event) messages.Event {
	return func(e messages.Event) messages.Event {
		log.Trace().Msgf("Got v: %+v", e)
		if v, ok := e.(messages.GetID); ok {

			return cmds.NewModify(aggregate, v.GetID(), values(v), destination, v.GetMessage().GroupID)
		}
		return nil

	}
}

func StartBasedOnGetID(aggregate string, destination string) func(messages.Event) messages.Event {
	return func(e messages.Event) messages.Event {
		if v, ok := e.(messages.GetID); ok {
			return cmds.NewStart(aggregate, v.GetID(), destination, v.GetMessage().GroupID)
		}
		return nil
	}
}

func CreateDefaultMessage(to func([]byte) (string, messages.Event, error)) VerifyAndParse {
	return func(e messages.Event, b []byte, m messages.Message) (bool, messages.Event, error) {

		mmt := m.MessageType()
		emt := e.MessageType()

		if m.GroupID == e.GetMessage().GroupID && mmt.Aggregate == emt.Aggregate {
			ff, r, err := to(b)
			if err != nil {
				return true, nil, err
			}
			if ff == mmt.Function {
				return true, r, nil
			} else {
				return true, nil, fmt.Errorf("wrong function %s (expected %s)", mmt.Function, ff)
			}
		}
		return true, nil, errors.New("incorrect aggregate or group")
	}

}

func OpenvasScanSuccess(e messages.Event, b []byte, m messages.Message) (bool, messages.Event, error) {
	basedOn, ok := e.(messages.GetID)
	if !ok {
		return false, nil, errors.New("unable parse message to messages.GetID")
	}
	mt := m.MessageType()
	switch strings.ToLower(mt.Function) {
	case "status":
		var status info.Status
		if err := json.Unmarshal(b, &status); err != nil {
			return false, nil, err
		}
		if status.ID != basedOn.GetID() {
			return false, nil, fmt.Errorf("status ID (%s) does not match scan id (%s)", status.ID, basedOn.GetID())
		}
		switch status.Status {
		case info.REQUESTED, info.QUEUED, info.INIT, info.RUNNING, info.STOPPING:
			return false, status, nil
		case info.FINISHED, info.STOPPED:
			return true, status, nil
		default:
			return false, nil, fmt.Errorf("status (%s) is not a success case", status.Status)
		}
	case "got":
		if mt.Aggregate != "result" {
			return false, nil, fmt.Errorf("aggregate (%s) does not match result", mt.Aggregate)
		}
		var result models.GotResult
		if err := json.Unmarshal(b, &result); err != nil {
			return false, nil, err
		}
		if result.ID != basedOn.GetID() {
			return false, nil, fmt.Errorf("id (%s) does not match expected ID (%s)", result.ID, basedOn.GetID())
		}
		return false, result, nil
	default:
		return false, nil, fmt.Errorf("invalid function (%s)", mt.Function)
	}

}

func OpenvasScanFailure(e messages.Event, b []byte, m messages.Message) (bool, messages.Event, error) {
	basedOn, ok := e.(messages.GetID)
	if !ok {
		return false, nil, errors.New("unable cast message to messages.GetID")
	}
	mt := m.MessageType()
	switch strings.ToLower(mt.Function) {
	case "status":
		var status info.Status
		if err := json.Unmarshal(b, &status); err != nil {
			return false, nil, err
		}
		if status.ID != basedOn.GetID() {
			return false, nil, fmt.Errorf("status ID (%s) does not match scan id (%s)", status.ID, basedOn.GetID())
		}
		switch status.Status {
		case info.FAILED, info.INTERRUPTED, info.STOPPED:
			return true, status, nil
		default:
			return false, nil, fmt.Errorf("status (%s) is not a fail case", status.Status)
		}
	case "failure":
		var c info.Failure
		if err := json.Unmarshal(b, &c); err != nil {
			return false, nil, err
		}
		return true, c, nil

	default:
		return false, nil, fmt.Errorf("invalid function (%s)", mt.Function)
	}

}

func CreatedParser(b []byte) (string, messages.Event, error) {
	var c info.Created
	if err := json.Unmarshal(b, &c); err != nil {
		return "", nil, err
	}
	return "created", c, nil
}

func ModifiedParser(b []byte) (string, messages.Event, error) {
	var c info.Modified
	if err := json.Unmarshal(b, &c); err != nil {
		return "", nil, err
	}
	return "modified", c, nil
}
func DeletedParser(b []byte) (string, messages.Event, error) {
	var c info.Deleted
	if err := json.Unmarshal(b, &c); err != nil {
		return "", nil, err
	}
	return "deleted", c, nil
}
func GotTargetParser(b []byte) (string, messages.Event, error) {
	var c models.GotTarget
	if err := json.Unmarshal(b, &c); err != nil {
		return "", nil, err
	}
	return "got", c, nil
}
func GotScanParser(b []byte) (string, messages.Event, error) {
	var c models.GotScan
	if err := json.Unmarshal(b, &c); err != nil {
		return "", nil, err
	}
	return "got", c, nil
}
func GotResultParser(b []byte) (string, messages.Event, error) {
	var c models.GotResult
	if err := json.Unmarshal(b, &c); err != nil {
		return "", nil, err
	}
	return "got", c, nil
}
func GotSensorParser(b []byte) (string, messages.Event, error) {
	var c models.GotSensor
	if err := json.Unmarshal(b, &c); err != nil {
		return "", nil, err
	}
	return "got", c, nil
}

func FailureParser(b []byte) (string, messages.Event, error) {
	var c info.Failure
	if err := json.Unmarshal(b, &c); err != nil {
		return "", nil, err
	}
	return "failure", c, nil
}

type Configuration struct {
	Context    string
	Out        chan<- *connection.SendResponse
	In         <-chan *connection.TopicData
	DownStream chan<- *Received
	Timeout    time.Duration
}

func From(
	c Configuration,
	msg messages.Event) (*Program, error) {
	if c.Timeout < 1 {
		log.Info().Msg("No timeout specified; setting it to 5 minutes per message")
		c.Timeout = 5 * time.Minute
	}
	if msg.GetMessage().GroupID == "" {
		return nil, errors.New("a program needs to have group id for identification of belonging messages")
	}

	var vs VerifyAndParse
	var vf VerifyAndParse
	switch v := msg.(type) {
	case cmds.Create:
		vs = CreateDefaultMessage(CreatedParser)
		vf = CreateDefaultMessage(FailureParser)
	case cmds.Get:
		var parser func([]byte) (string, messages.Event, error)
		switch v.MessageType().Aggregate {
		case "target":
			parser = GotTargetParser
		case "sensor":
			parser = GotSensorParser
		case "scan":
			parser = GotScanParser
		case "result":
			parser = GotResultParser
		default:
			return nil, fmt.Errorf("no known parser for %s", v.MessageType().Aggregate)
		}

		vs = CreateDefaultMessage(parser)
		vf = CreateDefaultMessage(FailureParser)
	case cmds.Delete:
		vs = CreateDefaultMessage(DeletedParser)
		vf = CreateDefaultMessage(FailureParser)
	case cmds.Modify:
		vs = CreateDefaultMessage(ModifiedParser)
		vf = CreateDefaultMessage(FailureParser)
	default:
		return nil, errors.New("unable to create Program from: %v; please use New instead")
	}
	return &Program{
		init:          msg,
		verifySuccess: vs,
		verifyFailure: vf,
		send:          c.Out,
		receive:       c.In,
		received:      c.DownStream,
		timeout:       c.Timeout,
	}, nil
}
