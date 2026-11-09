package sonos

import (
	"context"
	"encoding/xml"
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"
)

// Group is speakers that play together: a room on its own is a group of one.
// Whatever plays is said to the coordinator, which the others follow.
type Group struct {
	// ID is the uuid of the coordinator, which names the group for as long as
	// it lasts.
	ID          string   `json:"id"`
	Coordinator Player   `json:"coordinator"`
	Members     []Player `json:"members"`
}

// Name is how the group is shown: the room of the coordinator, and how many
// rooms play with it.
func (g Group) Name() string {
	if len(g.Members) <= 1 {
		return g.Coordinator.Room
	}
	return fmt.Sprintf("%s + %d", g.Coordinator.Room, len(g.Members)-1)
}

// Groups asks the speakers how they are grouped. Any speaker knows that of
// its whole household, so one is asked per household; the speakers given are
// what a search found or the configuration named, and one that is not
// grouped with any other asked so far is asked in its turn. Speakers whose
// state cannot be read are each taken as a group of their own.
func (c *Client) Groups(ctx context.Context, players []Player) ([]Group, error) {
	known := make(map[string]Player, len(players))
	for _, p := range players {
		known[p.UUID] = p
	}
	seen := map[string]bool{}
	var groups []Group
	var lastErr error
	for _, p := range players {
		if seen[p.UUID] {
			continue
		}
		found, placed, err := c.zoneGroups(ctx, p.Host)
		if err != nil {
			lastErr = err
			seen[p.UUID] = true
			groups = append(groups, Group{ID: p.UUID, Coordinator: p, Members: []Player{p}})
			continue
		}
		// a speaker that is part of a room rather than one of its own, a
		// surround or a subwoofer, answers a search as any other does; the
		// state places it, so it is not taken for a room
		for _, uuid := range placed {
			if !seen[uuid] && !slices.ContainsFunc(found, func(g Group) bool { return g.has(uuid) }) {
				seen[uuid] = true
			}
		}
		for _, g := range found {
			if seen[g.ID] {
				continue
			}
			for i, m := range g.Members {
				seen[m.UUID] = true
				g.Members[i].Model = known[m.UUID].Model
			}
			g.Coordinator = g.Members[0]
			groups = append(groups, g)
		}
		// a speaker its own state does not place, as where it is is not
		// believed, plays on its own as far as can be told
		if !seen[p.UUID] {
			seen[p.UUID] = true
			groups = append(groups, Group{ID: p.UUID, Coordinator: p, Members: []Player{p}})
		}
	}
	if len(groups) == 0 && lastErr != nil {
		return nil, lastErr
	}
	sort.Slice(groups, func(i, j int) bool {
		return strings.ToLower(groups[i].Coordinator.Room) < strings.ToLower(groups[j].Coordinator.Room)
	})
	return groups, nil
}

// has reports whether a speaker is a member of the group.
func (g Group) has(uuid string) bool {
	return slices.ContainsFunc(g.Members, func(m Player) bool { return m.UUID == uuid })
}

// zoneGroups reads the groups of the household of the speaker at host, and
// every speaker the state places, those that are part of a room included.
func (c *Client) zoneGroups(ctx context.Context, host string) ([]Group, []string, error) {
	out, err := c.call(ctx, host, zoneGroupTopology, "GetZoneGroupState")
	if err != nil {
		return nil, nil, err
	}
	groups, placed, err := parseZoneGroups(out["ZoneGroupState"])
	if err != nil {
		return nil, nil, err
	}
	// the state names where each speaker is; only the port of a speaker is
	// believed, as with an answer to a search
	kept := groups[:0]
	for _, g := range groups {
		if g.coordinatorAt(c.port) {
			members := g.Members[:0]
			for _, m := range g.Members {
				if _, port, err := net.SplitHostPort(m.Host); err == nil && port == c.port {
					members = append(members, m)
				}
			}
			g.Members = members
			kept = append(kept, g)
		}
	}
	return kept, placed, nil
}

func (g Group) coordinatorAt(port string) bool {
	_, p, err := net.SplitHostPort(g.Coordinator.Host)
	return err == nil && p == port
}

type zoneGroupXML struct {
	Coordinator string `xml:"Coordinator,attr"`
	Members     []struct {
		UUID       string `xml:"UUID,attr"`
		Location   string `xml:"Location,attr"`
		ZoneName   string `xml:"ZoneName,attr"`
		Invisible  string `xml:"Invisible,attr"`
		Satellites []struct {
			UUID string `xml:"UUID,attr"`
		} `xml:"Satellite"`
	} `xml:"ZoneGroupMember"`
}

// parseZoneGroups reads the state a speaker answers with: ZoneGroupState
// holding ZoneGroups, or on older firmware ZoneGroups alone. A member that is
// invisible, the second speaker of a stereo pair, and the satellites of a
// home theatre, its surrounds and its subwoofer, are part of a room rather
// than rooms of their own: they are left out of the groups, and only
// returned, with every other speaker the state names, as placed.
func parseZoneGroups(state string) ([]Group, []string, error) {
	var doc struct {
		Nested []zoneGroupXML `xml:"ZoneGroups>ZoneGroup"`
		Flat   []zoneGroupXML `xml:"ZoneGroup"`
	}
	if err := xml.Unmarshal([]byte(state), &doc); err != nil {
		return nil, nil, fmt.Errorf("sonos zone groups: %w", err)
	}
	found := append(doc.Nested, doc.Flat...)
	groups := make([]Group, 0, len(found))
	var placed []string
	for _, zg := range found {
		g := Group{ID: zg.Coordinator}
		for _, m := range zg.Members {
			placed = append(placed, m.UUID)
			for _, sat := range m.Satellites {
				placed = append(placed, sat.UUID)
			}
			if m.Invisible == "1" {
				continue
			}
			where, err := url.Parse(m.Location)
			if err != nil || where.Host == "" {
				continue
			}
			p := Player{UUID: m.UUID, Room: m.ZoneName, Host: where.Host}
			if m.UUID == zg.Coordinator {
				g.Coordinator = p
				g.Members = append([]Player{p}, g.Members...)
			} else {
				g.Members = append(g.Members, p)
			}
		}
		if g.Coordinator.UUID == "" {
			continue
		}
		groups = append(groups, g)
	}
	return groups, placed, nil
}
