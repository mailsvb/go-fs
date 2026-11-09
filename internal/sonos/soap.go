// Package sonos plays files on the Sonos speakers of the local network. It
// speaks the little of UPnP that takes: SSDP to find the speakers, the
// description each one serves to learn its room, and SOAP to the services on
// port 1400 to group, play, pause and set the volume.
//
// A speaker is not sent the audio. It is told the URL of a file, which it
// then fetches itself, so the file has to be served at an address the
// speaker reaches and that needs no login; the caller sees to that.
package sonos

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Port is the port a speaker serves its description and its services on.
const Port = "1400"

// maxReply bounds what is read of any answer of a speaker; a zone group
// state of a large household is a few dozen kilobytes.
const maxReply = 1 << 20

// Client talks to the speakers. The zero value is not usable; New makes one.
type Client struct {
	// HTTP is what the descriptions and the SOAP calls go through.
	HTTP *http.Client
	// Search is where a search is sent: the SSDP group, or in a test a
	// socket on the loopback interface that stands in for the speakers.
	Search string
	// port is the port an answer to a search has to name, Port but in a
	// test.
	port string
}

// New makes a client that reaches the speakers directly, never through a
// proxy, and gives up on one that does not answer within a few seconds.
func New() *Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		ResponseHeaderTimeout: 5 * time.Second,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
	}
	return &Client{
		HTTP: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
			// a speaker answers where it is asked; a redirect would be
			// someone else answering for it
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		Search: ssdpGroup,
		port:   Port,
	}
}

// Error is a fault a speaker answered a call with.
type Error struct {
	Action string
	// Code is the UPnP error code, 0 when the fault named none.
	Code        int
	Description string
}

func (e *Error) Error() string {
	text := fmt.Sprintf("sonos %s failed", e.Action)
	if e.Code != 0 {
		text += fmt.Sprintf(" with UPnP error %d", e.Code)
	}
	if meaning := e.meaning(); meaning != "" {
		text += ": " + meaning
	} else if e.Description != "" {
		text += ": " + e.Description
	}
	return text
}

// meaning is what the codes a speaker commonly answers with mean.
func (e *Error) meaning() string {
	switch e.Code {
	case 701:
		return "the speaker cannot do that in the state it is in"
	case 714:
		return "the speaker does not play this kind of file"
	case 716:
		return "the speaker could not find the file"
	case 800:
		return "the speaker is not the coordinator of its group"
	}
	return ""
}

// NotCoordinator reports whether err says the speaker called is a member of
// a group whose coordinator has to be called instead.
func NotCoordinator(err error) bool {
	var fault *Error
	return errors.As(err, &fault) && fault.Code == 800
}

// arg is one argument of a call, in the order the action declares them.
type arg struct {
	name, value string
}

// service is a SOAP service of a speaker: where it is and what it is.
type service struct {
	path, urn string
}

var (
	avTransport = service{"/MediaRenderer/AVTransport/Control",
		"urn:schemas-upnp-org:service:AVTransport:1"}
	groupRendering = service{"/MediaRenderer/GroupRenderingControl/Control",
		"urn:schemas-upnp-org:service:GroupRenderingControl:1"}
	zoneGroupTopology = service{"/ZoneGroupTopology/Control",
		"urn:schemas-upnp-org:service:ZoneGroupTopology:1"}
)

// call invokes an action of a service of the speaker at host, ip:port, and
// returns the arguments of the answer by name.
func (c *Client) call(ctx context.Context, host string, svc service, action string, args ...arg) (map[string]string, error) {
	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&body, `<u:%s xmlns:u="%s">`, action, svc.urn)
	for _, a := range args {
		fmt.Fprintf(&body, "<%s>", a.name)
		if err := xml.EscapeText(&body, []byte(a.value)); err != nil {
			return nil, err
		}
		fmt.Fprintf(&body, "</%s>", a.name)
	}
	fmt.Fprintf(&body, `</u:%s></s:Body></s:Envelope>`, action)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+host+svc.path, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPACTION", `"`+svc.urn+"#"+action+`"`)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sonos %s: %w", action, err)
	}
	defer func() { _ = res.Body.Close() }()
	reply, err := io.ReadAll(io.LimitReader(res.Body, maxReply))
	if err != nil {
		return nil, fmt.Errorf("sonos %s: %w", action, err)
	}
	if res.StatusCode != http.StatusOK {
		fault := &Error{Action: action}
		var env faultEnvelope
		if xml.Unmarshal(reply, &env) == nil {
			fault.Code = env.Code
			fault.Description = strings.TrimSpace(env.Description)
		}
		if fault.Code == 0 && fault.Description == "" {
			fault.Description = res.Status
		}
		return nil, fault
	}
	var env replyEnvelope
	if err := xml.Unmarshal(reply, &env); err != nil {
		return nil, fmt.Errorf("sonos %s: unreadable answer: %w", action, err)
	}
	if env.Body.Response.XMLName.Local != action+"Response" {
		return nil, fmt.Errorf("sonos %s: the answer is %q", action, env.Body.Response.XMLName.Local)
	}
	out := make(map[string]string, len(env.Body.Response.Args))
	for _, a := range env.Body.Response.Args {
		out[a.XMLName.Local] = a.Value
	}
	return out, nil
}

type replyEnvelope struct {
	Body struct {
		Response struct {
			XMLName xml.Name
			Args    []struct {
				XMLName xml.Name
				Value   string `xml:",chardata"`
			} `xml:",any"`
		} `xml:",any"`
	} `xml:"Body"`
}

type faultEnvelope struct {
	Code        int    `xml:"Body>Fault>detail>UPnPError>errorCode"`
	Description string `xml:"Body>Fault>detail>UPnPError>errorDescription"`
}
