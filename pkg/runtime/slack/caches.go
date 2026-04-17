package slack

import (
	"sync"
	"time"

	slackgo "github.com/slack-go/slack"
)

// channelCache resolves a Slack channel id (e.g. "C0ATGC…") to its human
// name (e.g. "orca-activity"). Lets routing yaml use readable "#foo" keys
// even though Slack events only carry channel ids.
type channelCache struct {
	mu      sync.Mutex
	client  *slackgo.Client
	entries map[string]channelEntry
	ttl     time.Duration
}

type channelEntry struct {
	name      string
	fetchedAt time.Time
}

func newChannelCache(c *slackgo.Client) *channelCache {
	return &channelCache{
		client:  c,
		entries: map[string]channelEntry{},
		ttl:     30 * time.Minute,
	}
}

// Name returns the channel name (without the leading "#") for a given id,
// or "" if the lookup fails. IM channels (DMs) return "" by design.
func (c *channelCache) Name(id string) string {
	if id == "" {
		return ""
	}
	c.mu.Lock()
	e, ok := c.entries[id]
	c.mu.Unlock()
	if ok && time.Since(e.fetchedAt) < c.ttl {
		return e.name
	}

	info, err := c.client.GetConversationInfo(&slackgo.GetConversationInfoInput{
		ChannelID: id,
	})
	if err != nil || info == nil {
		c.mu.Lock()
		c.entries[id] = channelEntry{name: "", fetchedAt: time.Now()}
		c.mu.Unlock()
		return ""
	}
	c.mu.Lock()
	c.entries[id] = channelEntry{name: info.Name, fetchedAt: time.Now()}
	c.mu.Unlock()
	return info.Name
}

// userCache resolves a Slack user id to a display name. TTL'd. API
// failures fall back to the id so callers always have something to show.
type userCache struct {
	mu      sync.Mutex
	client  *slackgo.Client
	entries map[string]userEntry
	ttl     time.Duration
}

type userEntry struct {
	name      string
	fetchedAt time.Time
}

func newUserCache(c *slackgo.Client) *userCache {
	return &userCache{
		client:  c,
		entries: map[string]userEntry{},
		ttl:     30 * time.Minute,
	}
}

func (u *userCache) Name(id string) string {
	u.mu.Lock()
	e, ok := u.entries[id]
	u.mu.Unlock()
	if ok && time.Since(e.fetchedAt) < u.ttl {
		return e.name
	}

	info, err := u.client.GetUserInfo(id)
	if err != nil || info == nil {
		u.mu.Lock()
		u.entries[id] = userEntry{name: id, fetchedAt: time.Now()}
		u.mu.Unlock()
		return id
	}
	name := info.Profile.DisplayName
	if name == "" {
		name = info.RealName
	}
	if name == "" {
		name = info.Name
	}
	if name == "" {
		name = id
	}
	u.mu.Lock()
	u.entries[id] = userEntry{name: name, fetchedAt: time.Now()}
	u.mu.Unlock()
	return name
}
