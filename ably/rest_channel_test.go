package ably_test

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ably/ably-go/ably/internal/ablyutil"

	"github.com/ably/ably-go/ably"
	"github.com/ably/ably-go/ably/ablytest"
	"github.com/ably/ably-go/ably/proto"
)

func TestRESTChannel(t *testing.T) {
	t.Parallel()
	app, err := ablytest.NewSandbox(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	options := app.Options()
	client, err := ably.NewREST(options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("Publish", func(ts *testing.T) {
		channel := client.Channels.Get("test_publish_channel")

		type dataSample struct {
			encoding string
			data     interface{}
		}
		m := make(map[string]dataSample)
		if ablytest.NoBinaryProtocol {
			m["string"] = dataSample{
				encoding: proto.UTF8,
				data:     "string",
			}
			m["binary"] = dataSample{
				encoding: proto.Base64,
				data:     []byte("string"),
			}
			m["json"] = dataSample{
				encoding: proto.JSON,
				data: map[string]interface{}{
					"key": "value",
				},
			}
		} else {
			m["string"] = dataSample{
				encoding: proto.UTF8,
				data:     "string",
			}
			m["binary"] = dataSample{
				encoding: proto.UTF8,
				data:     "string",
			}
			m["json"] = dataSample{
				encoding: proto.JSON,
				data: map[string]interface{}{
					"key": "value",
				},
			}
		}
		for k, v := range m {
			err := channel.Publish(context.Background(), k, v.data)
			if err != nil {
				ts.Fatal(err)
			}
		}
		ts.Run("is available in the history", func(ts *testing.T) {
			var messages []*ably.Message
			err := ablytest.AllPages(&messages, channel.History())
			if err != nil {
				ts.Fatal(err)
			}
			if len(messages) == 0 {
				ts.Fatal("expected messages")
			}
		})
	})

	t.Run("PublishMultiple", func(ts *testing.T) {
		encodingRESTChannel := client.Channels.Get("this?is#an?encoding#channel")
		messages := []*ably.Message{
			{Name: "send", Data: "test data 1"},
			{Name: "send", Data: "test data 2"},
		}
		err := encodingRESTChannel.PublishMultiple(context.Background(), messages)
		if err != nil {
			ts.Fatal(err)
		}
		var history []*ably.Message
		err = ablytest.AllPages(&history, encodingRESTChannel.History(ably.HistoryWithLimit(2)))
		if err != nil {
			ts.Fatal(err)
		}
		if len(history) != 2 {
			ts.Errorf("expected 2 messages got %d", len(history))
		}
	})

	t.Run("encryption", func(t *testing.T) {
		key, err := base64.StdEncoding.DecodeString("WUP6u0K7MXI5Zeo0VppPwg==")
		if err != nil {
			t.Fatal(err)
		}
		iv, err := base64.StdEncoding.DecodeString("HO4cYSP8LybPYBPZPHQOtg==")
		if err != nil {
			t.Fatal(err)
		}
		opts := []ably.ChannelOption{ably.ChannelWithCipher(proto.CipherParams{
			Key:       key,
			KeyLength: 128,
			IV:        iv,
			Algorithm: proto.AES,
		})}
		channelName := "encrypted_channel"
		channel := client.Channels.Get(channelName, opts...)
		sample := []struct {
			event, message string
		}{
			{"publish_0", "first message"},
			{"publish_1", "second message"},
		}
		for _, v := range sample {
			err := channel.Publish(context.Background(), v.event, v.message)
			if err != nil {
				t.Error(err)
			}
		}

		var msg []*ably.Message
		err = ablytest.AllPages(&msg, channel.History())
		if err != nil {
			t.Fatal(err)
		}
		sort.Slice(msg, func(i, j int) bool {
			return msg[i].Name < msg[j].Name
		})
		for k, v := range msg {
			e := sample[k]
			if v.Name != e.event {
				t.Errorf("expected %s got %s", e.event, v.Name)
			}
			if !reflect.DeepEqual(v.Data, e.message) {
				t.Errorf("expected %s got %v", e.message, v.Data)
			}
		}
	})

}

func TestIdempotentPublishing(t *testing.T) {
	t.Parallel()
	app, err := ablytest.NewSandboxWIthEnv(nil, ablytest.Environment)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	options := app.Options(ably.WithIdempotentRESTPublishing(true))
	client, err := ably.NewREST(options...)
	if err != nil {
		t.Fatal(err)
	}
	randomStr, err := ablyutil.BaseID()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("when ID is not included (#RSL1k2)", func(ts *testing.T) {
		channel := client.Channels.Get("idempotent_test_1")
		for range make([]struct{}, 3) {
			err := channel.Publish(context.Background(), "", randomStr)
			if err != nil {
				ts.Fatal(err)
			}
		}
		var history []*ably.Message
		err := ablytest.AllPages(&history, channel.History())
		if err != nil {
			ts.Fatal(err)
		}
		n := len(history)
		if n != 3 {
			ts.Errorf("expected %d got %d", 3, n)
		}
	})

	t.Run("when ID is included (#RSL1k2, #RSL1k5)", func(ts *testing.T) {
		channel := client.Channels.Get("idempotent_test_2")
		for range make([]struct{}, 3) {
			err := channel.PublishMultiple(context.Background(), []*ably.Message{
				{
					ID:   randomStr,
					Data: randomStr,
				},
			})
			if err != nil {
				ts.Fatal(err)
			}
		}
		var history []*ably.Message
		err := ablytest.AllPages(&history, channel.History())
		if err != nil {
			ts.Fatal(err)
		}
		n := len(history)
		if n != 1 {
			// three REST publishes result in only one message being published
			ts.Errorf("expected %d got %d", 1, n)
		}
		msg := history[0]
		if msg.ID != randomStr {
			ts.Errorf("expected id to be %s got %s", randomStr, msg.ID)
		}
	})

	t.Run("multiple messages in one publish operation (#RSL1k3)", func(ts *testing.T) {
		channel := client.Channels.Get("idempotent_test_3")
		err := channel.PublishMultiple(context.Background(), []*ably.Message{
			{
				ID:   randomStr,
				Data: randomStr,
			},
			{
				ID:   randomStr,
				Data: randomStr,
			},
			{
				ID:   randomStr,
				Data: randomStr,
			},
		})
		if err == nil {
			ts.Fatal("expected an error")
		}
		code := fmt.Sprint(ably.ErrInvalidPublishRequestInvalidClientSpecifiedID)
		if !strings.Contains(err.Error(), code) {
			ts.Errorf("expected error code %s got %s", code, err)
		}
	})

	t.Run("multiple messages in one publish operation with IDs following the required format described in RSL1k1 (#RSL1k3)", func(ts *testing.T) {
		channel := client.Channels.Get("idempotent_test_4")
		var m []*ably.Message
		for i := 0; i < 3; i++ {
			m = append(m, &ably.Message{
				ID: fmt.Sprintf("%s:%d", randomStr, i),
			})
		}
		err := channel.PublishMultiple(context.Background(), m)
		if err != nil {
			ts.Fatal(err)
		}
		var messages []*ably.Message
		err = ablytest.AllPages(&messages, channel.History())
		if err != nil {
			ts.Fatal(err)
		}
		n := len(messages)
		if n != 3 {
			ts.Errorf("expected %d got %d", 3, n)
		}

		// we need to sort so we we can easily test the serial in order.
		sort.Slice(messages, func(i, j int) bool {
			p := strings.Split(messages[i].ID, ":")
			p0 := strings.Split(messages[j].ID, ":")
			i1, err := strconv.Atoi(p[1])
			if err != nil {
				ts.Fatal(err)
			}
			i2, err := strconv.Atoi(p0[1])
			if err != nil {
				ts.Fatal(err)
			}
			return i1 < i2
		})
		for k, v := range messages {
			p := strings.Split(v.ID, ":")
			id, err := strconv.Atoi(p[1])
			if err != nil {
				ts.Fatal(err)
			}
			if id != k {
				ts.Errorf("expected serial to be %d got %d", k, id)
			}
		}
	})

	t.Run("the ID is populated with a random ID and serial 0 from this lib (#RSL1k1)", func(ts *testing.T) {
		channel := client.Channels.Get("idempotent_test_5")
		err := channel.Publish(context.Background(), "event", "")
		if err != nil {
			ts.Fatal(err)
		}
		var m []*ably.Message
		err = ablytest.AllPages(&m, channel.History())
		if err != nil {
			ts.Fatal(err)
		}
		if len(m) != 1 {
			ts.Fatalf("expected %d got %d", 1, len(m))
		}
		message := m[0]
		if message.ID == "" {
			ts.Fatal("expected message id not to be empty")
		}
		pattern := `^[A-Za-z0-9\+\/]+:0$`
		re := regexp.MustCompile(pattern)
		if !re.MatchString(message.ID) {
			ts.Fatalf("expected id %s to match pattern %q", message.ID, pattern)
		}
		baseID := strings.Split(message.ID, ":")[0]
		v, err := base64.StdEncoding.DecodeString(baseID)
		if err != nil {
			ts.Fatal(err)
		}
		if len(v) != 9 {
			ts.Errorf("expected %d bytes git %d", 9, len(v))
		}
	})

	t.Run("publishing a batch of messages", func(ts *testing.T) {
		channel := client.Channels.Get("idempotent_test_6")
		name := "event"
		err := channel.PublishMultiple(context.Background(), []*ably.Message{
			{Name: name},
			{Name: name},
			{Name: name},
		})
		if err != nil {
			ts.Fatal(err)
		}
		var m []*ably.Message
		err = ablytest.AllPages(&m, channel.History())
		if err != nil {
			ts.Fatal(err)
		}
		if len(m) != 3 {
			ts.Fatalf("expected %d got %d", 1, len(m))
		}
		pattern := `^[A-Za-z0-9\+\/]+:\d$`
		re := regexp.MustCompile(pattern)
		for _, message := range m {
			if message.ID == "" {
				ts.Fatal("expected message id not to be empty")
			}
			if !re.MatchString(message.ID) {
				ts.Fatalf("expected id %s to match pattern %q", message.ID, pattern)
			}
			baseID := strings.Split(message.ID, ":")[0]
			v, err := base64.StdEncoding.DecodeString(baseID)
			if err != nil {
				ts.Fatal(err)
			}
			if len(v) != 9 {
				ts.Errorf("expected %d bytes git %d", 9, len(v))
			}
		}
	})
}

func TestIdempotent_retry(t *testing.T) {
	t.Parallel()
	app, err := ablytest.NewSandboxWIthEnv(nil, ablytest.Environment)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	randomStr, err := ablyutil.BaseID()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("when there is a network failure triggering an automatic retry (#RSL1k4)", func(ts *testing.T) {
		var retryCount int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			retryCount++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		// set up the proxy to forward the second retry to the correct endpoint,
		// failing all others via the test server
		fallbackHosts := []string{"fallback0", "fallback1", "fallback2"}
		nopts := []ably.ClientOption{
			ably.WithEnvironment(ablytest.Environment),
			ably.WithTLS(false),
			ably.WithFallbackHosts(fallbackHosts),
			ably.WithIdempotentRESTPublishing(true),
			ably.WithUseTokenAuth(true),
		}

		serverURL, _ := url.Parse(server.URL)
		defaultURL, _ := url.Parse(ably.ApplyOptionsWithDefaults(nopts...).RestURL())
		proxy := func(r *http.Request) (*url.URL, error) {
			if !strings.HasPrefix(r.URL.Path, "/channels/") {
				// this is to handle token requests
				// set the Host in the request to the intended destination
				r.Host = defaultURL.Hostname()
				return defaultURL, nil
			}
			if retryCount < 2 {
				// ensure initial requests fail
				return serverURL, nil
			} else {
				// allow subsequent requests tos ucceed
				// set the Host in the request to the intended destination
				r.Host = defaultURL.Hostname()
				return defaultURL, nil
			}
		}
		nopts = append(nopts, ably.WithHTTPClient(&http.Client{
			Transport: &http.Transport{
				Proxy:        proxy,
				TLSNextProto: map[string]func(authority string, c *tls.Conn) http.RoundTripper{},
			},
		}))

		client, err := ably.NewREST(app.Options(nopts...)...)
		if err != nil {
			ts.Fatal(err)
		}

		ts.Run("two REST publish retries result in only one message being published'", func(ts *testing.T) {
			channel := client.Channels.Get("idempotent_test_fallback")
			err = channel.Publish(context.Background(), "", randomStr)
			if err != nil {
				ts.Error(err)
			}
			if retryCount != 2 {
				ts.Errorf("expected %d retry attempts got %d", 2, retryCount)
			}
			var m []*ably.Message
			err = ablytest.AllPages(&m, channel.History())
			if err != nil {
				ts.Fatal(err)
			}
			if len(m) != 1 {
				ts.Errorf("expected %d messages got %d", 1, len(m))
			}
		})
		ts.Run("or multiple messages in one publish operation'", func(ts *testing.T) {
			retryCount = 0
			channel := client.Channels.Get("idempotent_test_fallback_1")
			msgs := []*ably.Message{
				{Data: randomStr},
				{Data: randomStr},
				{Data: randomStr},
			}
			for range msgs {
				channel.PublishMultiple(context.Background(), msgs)
			}
			var m []*ably.Message
			err := ablytest.AllPages(&m, channel.History())
			if err != nil {
				ts.Fatal(err)
			}
			if len(m) != len(msgs) {
				ts.Errorf("expected %d messages got %d", len(msgs), len(m))
			}
		})
	})
}

func TestRSL1f1(t *testing.T) {
	t.Parallel()
	app, err := ablytest.NewSandbox(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	opts := app.Options()
	// RSL1f
	opts = append(opts, ably.WithUseTokenAuth(false))
	client, err := ably.NewREST(opts...)
	if err != nil {
		t.Fatal(err)
	}
	channel := client.Channels.Get("RSL1f")
	id := "any_client_id"
	var msgs []*ably.Message
	size := 10
	for i := 0; i < size; i++ {
		msgs = append(msgs, &ably.Message{
			ClientID: id,
			Data:     fmt.Sprint(i),
		})
	}
	err = channel.PublishMultiple(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	var m []*ably.Message
	err = ablytest.AllPages(&m, channel.History())
	if err != nil {
		t.Fatal(err)
	}
	n := len(m)
	if n != size {
		t.Errorf("expected %d messages got %d", size, n)
	}
	for _, v := range m {
		if v.ClientID != id {
			t.Errorf("expected clientId %s got %s data:%v", id, v.ClientID, v.Data)
		}
	}
}

func TestRSL1g(t *testing.T) {
	t.Parallel()
	app, err := ablytest.NewSandbox(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	opts := append(app.Options(),
		ably.WithUseTokenAuth(true),
	)
	clientID := "some_client_id"
	opts = append(opts, ably.WithClientID(clientID))
	client, err := ably.NewREST(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("RSL1g1b", func(ts *testing.T) {
		channel := client.Channels.Get("RSL1g1b")
		err := channel.PublishMultiple(context.Background(), []*ably.Message{
			{Name: "some 1"},
			{Name: "some 2"},
			{Name: "some 3"},
		})
		if err != nil {
			ts.Fatal(err)
		}
		var history []*ably.Message
		err = ablytest.AllPages(&history, channel.History())
		if err != nil {
			ts.Fatal(err)
		}
		for _, m := range history {
			if m.ClientID != clientID {
				ts.Errorf("expected %s got %s", clientID, m.ClientID)
			}
		}
	})
	t.Run("RSL1g2", func(ts *testing.T) {
		channel := client.Channels.Get("RSL1g2")
		err := channel.PublishMultiple(context.Background(), []*ably.Message{
			{Name: "1", ClientID: clientID},
			{Name: "2", ClientID: clientID},
			{Name: "3", ClientID: clientID},
		})
		if err != nil {
			ts.Fatal(err)
		}
		var history []*ably.Message
		err = ablytest.AllPages(&history, channel.History())
		if err != nil {
			ts.Fatal(err)
		}
		for _, m := range history {
			if m.ClientID != clientID {
				ts.Errorf("expected %s got %s", clientID, m.ClientID)
			}
		}
	})
	t.Run("RSL1g3", func(ts *testing.T) {
		channel := client.Channels.Get("RSL1g3")
		err := channel.PublishMultiple(context.Background(), []*ably.Message{
			{Name: "1", ClientID: clientID},
			{Name: "2", ClientID: "other client"},
			{Name: "3", ClientID: clientID},
		})
		if err == nil {
			ts.Fatal("expected an error")
		}
	})
}

func TestHistory_RSL2_RSL2b3(t *testing.T) {
	t.Parallel()

	for _, limit := range []int{2, 3, 20} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			t.Parallel()
			app, rest := ablytest.NewREST()
			defer app.Close()
			channel := rest.Channels.Get("test")

			fixtures := historyFixtures()
			channel.PublishMultiple(context.Background(), fixtures)

			err := ablytest.TestPagination(
				reverseMessages(fixtures),
				channel.History(ably.HistoryWithLimit(limit)),
				limit,
				ablytest.PaginationWithEqual(messagesEqual),
			)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHistory_Direction_RSL2b2(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		direction ably.Direction
		expected  []*ably.Message
	}{
		{
			direction: ably.Backwards,
			expected:  reverseMessages(historyFixtures()),
		},
		{
			direction: ably.Forwards,
			expected:  historyFixtures(),
		},
	} {
		c := c
		t.Run(fmt.Sprintf("direction=%v", c.direction), func(t *testing.T) {
			app, rest := ablytest.NewREST()
			defer app.Close()
			channel := rest.Channels.Get("test")

			fixtures := historyFixtures()
			channel.PublishMultiple(context.Background(), fixtures)

			expected := c.expected

			err := ablytest.TestPagination(expected, channel.History(
				ably.HistoryWithLimit(len(expected)),
				ably.HistoryWithDirection(c.direction),
			), 100, ablytest.PaginationWithEqual(messagesEqual))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func historyFixtures() []*ably.Message {
	var fixtures []*ably.Message
	for i := 0; i < 10; i++ {
		fixtures = append(fixtures, &ably.Message{Name: fmt.Sprintf("msg%d", i)})
	}
	return fixtures
}

func reverseMessages(msgs []*ably.Message) []*ably.Message {
	var reversed []*ably.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		reversed = append(reversed, msgs[i])
	}
	return reversed
}

func messagesEqual(x, y interface{}) bool {
	mx, my := x.(*ably.Message), y.(*ably.Message)
	return mx.Name == my.Name && reflect.DeepEqual(mx.Data, my.Data)
}
