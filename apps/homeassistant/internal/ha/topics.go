// Package ha turns the device's framework state into the Home Assistant
// entity model and the MQTT traffic that model needs.
//
// It contains no transport and no framework calls: the MQTT session lives in
// package mqtt and the device access lives in package hw. Keeping the Home
// Assistant contract here makes it unit-testable without a broker, a device or
// a sandbox.
package ha

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Defaults for the configurable topic roots.
const (
	DefaultDiscoveryPrefix = "homeassistant"
	DefaultBaseTopic       = "capsulad"
)

// Topic limits. MQTT allows 65535 bytes per topic; these bounds keep a
// misconfigured prefix from producing unusable topics and keep discovery
// payloads small.
const (
	maxTopicLength = 512
	maxSlugLength  = 32
)

// Topics is the topic layout for one bridge run:
//
//	<discovery>/<component>/<node>/<slug>/config   discovery payload (retained)
//	<base>/<node>/status                           availability, "online"/"offline"
//	<base>/<node>/<slug>/state                     entity state
//	<base>/<node>/<slug>/set                       entity command
type Topics struct {
	DiscoveryPrefix string
	BaseTopic       string
	NodeID          string
}

// NewTopics validates and normalizes the topic layout. An empty discovery
// prefix or base topic selects the default; an empty or unusable node id is an
// error, because the node id is the device's Home Assistant identity.
func NewTopics(discoveryPrefix, baseTopic, nodeID string) (Topics, error) {
	t := Topics{DiscoveryPrefix: discoveryPrefix, BaseTopic: baseTopic, NodeID: nodeID}
	if t.DiscoveryPrefix == "" {
		t.DiscoveryPrefix = DefaultDiscoveryPrefix
	}
	if t.BaseTopic == "" {
		t.BaseTopic = DefaultBaseTopic
	}
	if err := validPrefix("discovery prefix", t.DiscoveryPrefix); err != nil {
		return Topics{}, err
	}
	if err := validPrefix("base topic", t.BaseTopic); err != nil {
		return Topics{}, err
	}
	if t.NodeID == "" {
		return Topics{}, fmt.Errorf("ha: node id is empty")
	}
	if slug := Slug(t.NodeID); slug == "" {
		return Topics{}, fmt.Errorf("ha: node id %q has no usable characters", t.NodeID)
	}
	if !validTopic(t.NodeID) || strings.Contains(t.NodeID, "/") {
		return Topics{}, fmt.Errorf("ha: node id %q is not a single valid topic level", t.NodeID)
	}
	return t, nil
}

// StatusTopic is the retained availability topic.
func (t Topics) StatusTopic() string {
	return t.BaseTopic + "/" + t.NodeID + "/status"
}

// StateTopic is one entity's state topic.
func (t Topics) StateTopic(slug string) string {
	return t.entityTopic(slug, "state")
}

// SetTopic is one entity's command topic.
func (t Topics) SetTopic(slug string) string {
	return t.entityTopic(slug, "set")
}

// AttributesTopic is the JSON attributes topic for an entity.
func (t Topics) AttributesTopic(slug string) string {
	return t.entityTopic(slug, "attributes")
}

// DiscoveryTopic is one entity's discovery configuration topic.
func (t Topics) DiscoveryTopic(component, slug string) string {
	return t.DiscoveryPrefix + "/" + component + "/" + t.NodeID + "/" + slug + "/config"
}

func (t Topics) entityTopic(slug, suffix string) string {
	return t.BaseTopic + "/" + t.NodeID + "/" + slug + "/" + suffix
}

// UniqueID is the entity's stable Home Assistant unique id.
func (t Topics) UniqueID(slug string) string { return t.NodeID + "_" + slug }

// ValidTopic reports whether s can be published on: non-empty levels, no
// wildcards, valid UTF-8 and within the length bound.
func ValidTopic(s string) bool {
	if s == "" || len(s) > maxTopicLength || !utf8.ValidString(s) {
		return false
	}
	if strings.ContainsAny(s, "+#\x00") {
		return false
	}
	return !strings.HasPrefix(s, "/") && !strings.HasSuffix(s, "/") && !strings.Contains(s, "//")
}

func validTopic(s string) bool { return ValidTopic(s) }

// validPrefix rejects a prefix that would build an unusable topic, and the
// wildcards that make a topic a filter instead of a destination.
func validPrefix(name, value string) error {
	if !ValidTopic(value) {
		return fmt.Errorf("ha: %s %q is not a valid topic prefix", name, value)
	}
	return nil
}

// Slug converts arbitrary text into a topic level and entity-id fragment:
// lower case ASCII letters, digits and underscores. A device id with other
// characters (dots, colons, spaces) therefore still yields a stable, usable
// slug, and the mapping is deterministic so a restarted app keeps the same
// entity ids.
func Slug(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
			continue
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			lastUnderscore = false
			continue
		}
		if !lastUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			lastUnderscore = true
		}
		if b.Len() > maxSlugLength {
			break
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > maxSlugLength {
		out = strings.Trim(out[:maxSlugLength], "_")
	}
	return out
}
