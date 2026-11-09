package sonos

import (
	"context"
	"encoding/xml"
	"strconv"
	"strings"
)

// Track is what a speaker is told of the file it plays, which its app shows.
type Track struct {
	Title  string
	Artist string
	Album  string
	// MIME is the type of the file, audio/mpeg and the like.
	MIME string
}

// didl is the DIDL-Lite description of a file at uri, as the metadata of
// SetAVTransportURI carries it.
func didl(uri string, t Track) string {
	var b strings.Builder
	esc := func(s string) {
		_ = xml.EscapeText(&b, []byte(s))
	}
	b.WriteString(`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" ` +
		`xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" ` +
		`xmlns:r="urn:schemas-rinconnetworks-com:metadata-1-0/" ` +
		`xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/">` +
		`<item id="-1" parentID="-1" restricted="true">`)
	b.WriteString(`<res protocolInfo="http-get:*:`)
	esc(t.MIME)
	b.WriteString(`:*">`)
	esc(uri)
	b.WriteString(`</res><dc:title>`)
	esc(t.Title)
	b.WriteString(`</dc:title>`)
	if t.Artist != "" {
		b.WriteString(`<dc:creator>`)
		esc(t.Artist)
		b.WriteString(`</dc:creator>`)
	}
	if t.Album != "" {
		b.WriteString(`<upnp:album>`)
		esc(t.Album)
		b.WriteString(`</upnp:album>`)
	}
	b.WriteString(`<upnp:class>object.item.audioItem.musicTrack</upnp:class></item></DIDL-Lite>`)
	return b.String()
}

var instance = arg{"InstanceID", "0"}

// Play has the group whose coordinator is at host play the file at uri,
// in place of whatever it played; its queue is left as it was.
func (c *Client) Play(ctx context.Context, host, uri string, t Track) error {
	if _, err := c.call(ctx, host, avTransport, "SetAVTransportURI", instance,
		arg{"CurrentURI", uri}, arg{"CurrentURIMetaData", didl(uri, t)}); err != nil {
		return err
	}
	return c.Resume(ctx, host)
}

// Join has the speaker at host leave the group it is in and play along with
// the group whose coordinator has the id coordinator. The speakers that are
// part of its room, the satellites of a home theatre or the second speaker of
// a stereo pair, come along. The grouping lasts until it is changed, here or
// in the Sonos app.
func (c *Client) Join(ctx context.Context, host, coordinator string) error {
	_, err := c.call(ctx, host, avTransport, "SetAVTransportURI", instance,
		arg{"CurrentURI", "x-rincon:" + coordinator}, arg{"CurrentURIMetaData", ""})
	return err
}

// Leave has the speaker at host leave its group and play on its own.
func (c *Client) Leave(ctx context.Context, host string) error {
	_, err := c.call(ctx, host, avTransport, "BecomeCoordinatorOfStandaloneGroup", instance)
	return err
}

// Resume plays on what the group was playing.
func (c *Client) Resume(ctx context.Context, host string) error {
	_, err := c.call(ctx, host, avTransport, "Play", instance, arg{"Speed", "1"})
	return err
}

// Pause pauses the group.
func (c *Client) Pause(ctx context.Context, host string) error {
	_, err := c.call(ctx, host, avTransport, "Pause", instance)
	return err
}

// Stop stops the group.
func (c *Client) Stop(ctx context.Context, host string) error {
	_, err := c.call(ctx, host, avTransport, "Stop", instance)
	return err
}

// State is what a group is doing.
type State struct {
	// Transport is PLAYING, PAUSED_PLAYBACK, STOPPED or TRANSITIONING.
	Transport string `json:"transport"`
	// Title is the title of what it plays, when its metadata names one.
	Title string `json:"title,omitempty"`
	// URI is what it plays.
	URI string `json:"-"`
	// Volume is the volume of the group, 0 to 100.
	Volume int `json:"volume"`
	// Position is how far into what it plays the group is, and Duration
	// how long that is, in seconds; 0 where the speaker does not say.
	Position int `json:"position"`
	Duration int `json:"duration"`
}

// State reads what the group whose coordinator is at host is doing.
func (c *Client) State(ctx context.Context, host string) (State, error) {
	info, err := c.call(ctx, host, avTransport, "GetTransportInfo", instance)
	if err != nil {
		return State{}, err
	}
	st := State{Transport: info["CurrentTransportState"]}
	if pos, err := c.call(ctx, host, avTransport, "GetPositionInfo", instance); err == nil {
		st.URI = pos["TrackURI"]
		st.Title = titleOf(pos["TrackMetaData"])
		st.Position = clock(pos["RelTime"])
		st.Duration = clock(pos["TrackDuration"])
	}
	if vol, err := c.GroupVolume(ctx, host); err == nil {
		st.Volume = vol
	}
	return st, nil
}

// clock is the seconds of a time as a speaker gives it, H:MM:SS, 0 for
// anything else, NOT_IMPLEMENTED of a stream with no end among it.
func clock(text string) int {
	parts := strings.Split(strings.TrimSpace(text), ":")
	if len(parts) != 3 {
		return 0
	}
	total := 0
	for _, part := range parts {
		// a fraction of a second, 0:01:02.500, is left out
		whole, _, _ := strings.Cut(part, ".")
		n, err := strconv.Atoi(whole)
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + n
	}
	return total
}

// titleOf is the dc:title of a DIDL-Lite description, "" where there is none.
func titleOf(meta string) string {
	if meta == "" || meta == "NOT_IMPLEMENTED" {
		return ""
	}
	var doc struct {
		Item struct {
			Title string `xml:"http://purl.org/dc/elements/1.1/ title"`
		} `xml:"item"`
	}
	if xml.Unmarshal([]byte(meta), &doc) != nil {
		return ""
	}
	return strings.TrimSpace(doc.Item.Title)
}

// GroupVolume reads the volume of the group, 0 to 100.
func (c *Client) GroupVolume(ctx context.Context, host string) (int, error) {
	out, err := c.call(ctx, host, groupRendering, "GetGroupVolume", instance)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out["CurrentVolume"])
}

// SetGroupVolume sets the volume of the group, keeping the volumes of its
// rooms in the proportion they were in.
func (c *Client) SetGroupVolume(ctx context.Context, host string, volume int) error {
	volume = min(max(volume, 0), 100)
	// the proportions are taken from a snapshot, which has to be made first
	if _, err := c.call(ctx, host, groupRendering, "SnapshotGroupVolume", instance); err != nil {
		return err
	}
	_, err := c.call(ctx, host, groupRendering, "SetGroupVolume", instance,
		arg{"DesiredVolume", strconv.Itoa(volume)})
	return err
}
