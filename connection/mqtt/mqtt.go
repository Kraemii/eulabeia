// Copyright (C) 2021 Greenbone Networks GmbH
//
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// package mqtt contains the mqtt implementation of connection
package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	"github.com/greenbone/eulabeia/connection"
)

type MQTT struct {
	client            *paho.Client
	connectProperties *paho.Connect
	qos               byte
	preprocessor      []connection.Preprocessor
}

func (m MQTT) Close() error {
	return m.client.Disconnect(&paho.Disconnect{ReasonCode: 0})
}

func (m MQTT) Preprocess(topic string, message []byte) ([]connection.TopicData, bool) {
	var td []connection.TopicData
	handled := false
	for _, p := range m.preprocessor {
		if r, ok := p.Preprocess(topic, message); ok {
			handled = true
			td = append(td, r...)
		}
	}
	return td, handled
}

func (m MQTT) register(topic string, handler connection.OnMessage) error {

	m.client.Router.RegisterHandler(topic, func(p *paho.Publish) {
		log.Printf("[%s] retrieved message: %s", topic, string(p.Payload))
		if msgs, ok := m.Preprocess(topic, p.Payload); ok {
			for _, t := range msgs {
				m.client.Router.Route(&packets.Publish{
					Payload:    t.Message,
					Topic:      t.Topic,
					Properties: p.Packet().Properties,
					PacketID:   p.PacketID,
					QoS:        p.QoS,
					Duplicate:  p.Packet().Duplicate,
					Retain:     p.Retain,
				})
			}
		} else {
			message, err := handler.On(topic, p.Payload)
			if err != nil {
				panic(err)
			}
			if message != nil {
				err = m.Publish(message.Topic, message.MSG)
				if err != nil {
					panic(err)
				}
			}
		}

	})

	_, err := m.client.Subscribe(context.Background(), &paho.Subscribe{
		// we need NoLocal otherwise we would consum our own messages
		// again and ack them.
		Subscriptions: map[string]paho.SubscribeOptions{
			topic: {QoS: m.qos, NoLocal: true},
		},
	},
	)
	return err
}

func (m MQTT) Subscribe(handler map[string]connection.OnMessage) error {
	for topic, h := range handler {
		if err := m.register(topic, h); err != nil {
			return err
		}
	}
	return nil
}

func (m MQTT) Publish(topic string, message interface{}) error {
	b, err := json.Marshal(message)
	if err != nil {
		return err
	}
	log.Printf("[%s] Publishing message: %s", topic, string(b))
	props := &paho.PublishProperties{}
	pb := &paho.Publish{
		Topic:      topic,
		QoS:        m.qos,
		Payload:    b,
		Properties: props,
	}
	_, err = m.client.Publish(context.Background(), pb)
	return err
}

func (m MQTT) Connect() error {
	ca, err := m.client.Connect(context.Background(), m.connectProperties)
	if err != nil {
		return err
	}
	if ca.ReasonCode != 0 {
		return fmt.Errorf("failed to connect to %s : %d - %s", m.client.Conn.RemoteAddr().String(), ca.ReasonCode, ca.Properties.ReasonString)
	}
	return nil
}

type LastWillMessage struct {
	Topic string
	MSG   interface{}
}

func (lwm LastWillMessage) asBytes() ([]byte, error) {
	return json.Marshal(lwm.MSG)
}

func New(server string,
	clientid string,
	username string,
	password string,
	lwm *LastWillMessage,
	preprocessor []connection.Preprocessor) (connection.PubSub, error) {

	conn, err := net.Dial("tcp", server)
	if err != nil {
		return nil, err
	}
	c := paho.NewClient(paho.ClientConfig{
		Router: paho.NewStandardRouter(),
		Conn:   conn,
	})

	cp := &paho.Connect{
		KeepAlive:  30,
		ClientID:   clientid,
		CleanStart: true,
		Username:   username,
		Password:   []byte(password),
	}
	if lwm != nil {
		if b, err := lwm.asBytes(); err != nil {
			return nil, err
		} else {
			cp.WillMessage = &paho.WillMessage{
				Topic:   lwm.Topic,
				QoS:     1,
				Payload: b,
			}
		}
	}
	if username != "" {
		cp.UsernameFlag = true
	}
	if password != "" {
		cp.PasswordFlag = true
	}
	return &MQTT{
		client:            c,
		connectProperties: cp,
		qos:               1,
		preprocessor:      preprocessor,
	}, nil
}
