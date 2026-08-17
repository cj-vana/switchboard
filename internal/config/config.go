// Package config reads the user's tier ladder and slot bindings.
//
// Tiers are an ordered quality-and-compute policy ladder, identified as t1
// through tN with user-assignable labels. The ordering is the user's intent,
// not a claim that model capability is globally one-dimensional: two tiers may
// bind the same model at different effort, and a cloud-hosted small model may
// outrun a local large one (§3.1).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/credential"
	"github.com/cj-vana/switchboard/internal/provider"
)

const FileName = "config.toml"

// MaxTiers is a soft ceiling. §21.1 leaves open whether the eval set and the
// interface stay comprehensible past roughly six tiers; refusing outright
// would answer a question the design deliberately left open, so this is high
// enough not to constrain experiments and low enough to catch a typo.
const MaxTiers = 32

// Tier binds one rung of the ladder to a route target.
type Tier struct {
	ID     string
	Label  string
	Target provider.RouteTarget

	// Fallbacks is the ordered list of targets that may serve this tier when
	// its primary cannot be reached (§5.4). Every entry was written into the
	// config by the user, which is what makes each an approved destination;
	// the substitution still renders before content is sent. Entries use the
	// provider's default serving surface.
	Fallbacks []provider.RouteTarget
}

func (t Tier) String() string {
	if t.Label == "" {
		return fmt.Sprintf("%s  %s", t.ID, t.Target.ID())
	}
	return fmt.Sprintf("%s  %-10s %s", t.ID, t.Label, t.Target.ID())
}

type Config struct {
	// Tiers is ordered ascending by the user's intended quality and compute
	// policy. It may be empty, which means no ladder is configured.
	Tiers []Tier

	// Slots bind named roles to a model or to a tier alias such as "t1".
	Slots map[string]string

	// Auth is keyed by provider name.
	Auth map[string]credential.Settings

	// Providers holds per-provider endpoint overrides, keyed by provider name.
	Providers map[string]ProviderSettings

	// UpdateCheck controls the release check the TUI runs at startup. It is
	// operational traffic, not telemetry (§16/§18): one request naming only the
	// running version. Default on; [updates] check = false or
	// SB_NO_UPDATE_CHECK=1 turns it off.
	UpdateCheck bool

	// UpdateAuto installs a newer release in the background when the startup
	// check finds one, leaving the running process alone; the new binary runs
	// on the next start. Default on. Installs owned by a package manager are
	// detected and never touched regardless of this setting (§18).
	UpdateAuto bool

	// UpdateChannel is "stable" (release tags only, the default) or "beta"
	// (prereleases count too).
	UpdateChannel string

	// CompactAuto summarizes the session into a fresh context automatically
	// when the window crosses CompactAtPercent. Default on: the alternative
	// is a session that works until the moment it cannot, with the failure
	// arriving as a provider error instead of a visible handoff.
	CompactAuto bool

	// CompactAtPercent is how full the window gets before auto-compaction,
	// as a percentage. Default 85.
	CompactAtPercent int

	// Theme is the TUI color theme, persisted so /theme survives a restart.
	// Empty means the built-in default; the TUI owns what names are valid.
	Theme string

	// Notify rings the terminal bell when a turn finishes or a permission
	// ask arrives, so a session left in another pane says when it needs its
	// person. Nil is the default, which is on: a config built in code and
	// saved must not quietly persist an opinion nobody stated. Read it
	// through NotifyOn.
	Notify *bool

	// Budget is a per-session dollar ceiling, persisted so /budget survives a
	// restart. Zero means no ceiling. It governs what the catalog prices in
	// dollars; a local rung consumes nothing scarce and a plan rung consumes
	// quota, and neither is what this bounds (§4, §15).
	Budget catalog.Money

	Path string
}

type ProviderSettings struct {
	BaseURL string
}

// ProviderFor returns the settings for a provider, which is the zero value when
// none are configured: every adapter has a default address.
func (c *Config) ProviderFor(name string) ProviderSettings {
	return c.Providers[name]
}

// AuthFor returns the credential settings for a provider, which is the zero
// value when none are configured: the default chain still works, because the
// environment and the platform store need no configuration.
func (c *Config) AuthFor(providerName string) credential.Settings {
	return c.Auth[providerName]
}

// NotifyOn is how the bell setting is read: absent means on.
func (c *Config) NotifyOn() bool {
	return c.Notify == nil || *c.Notify
}

// Default returns the tier a session starts on. The bottom of the ladder is
// the deliberate default: an escalation the user can see beats a silent spend
// they cannot (design principle 3).
func (c *Config) Default() (Tier, bool) {
	if len(c.Tiers) == 0 {
		return Tier{}, false
	}
	return c.Tiers[0], true
}

func (c *Config) Tier(id string) (Tier, bool) {
	for _, t := range c.Tiers {
		if t.ID == id {
			return t, true
		}
	}
	return Tier{}, false
}

type file struct {
	Tiers     map[string]tierEntry     `toml:"tiers"`
	Slots     map[string]string        `toml:"slots"`
	Auth      map[string]authEntry     `toml:"auth"`
	Providers map[string]providerEntry `toml:"providers"`
	Updates   updatesEntry             `toml:"updates"`
	Compact   compactEntry             `toml:"compact"`
	UI        uiEntry                  `toml:"ui"`
	Limits    limitsEntry              `toml:"limits"`
}

// limitsEntry holds the spending ceiling. Money's own text form is what the
// file reads and writes, so the value is "2.50", not a count of micro-dollars.
type limitsEntry struct {
	Budget catalog.Money `toml:"budget,omitempty"`
}

// compactEntry holds the auto-compaction settings. Auto is a *bool so
// "absent" and "explicitly off" are different facts: the default is on.
type compactEntry struct {
	Auto      *bool `toml:"auto,omitempty"`
	AtPercent int   `toml:"at_percent,omitempty"`
}

// uiEntry holds presentation settings. They live in the config rather than a
// separate state file because the TUI writes this file anyway, and two files
// that both mean "how sb behaves for this user" is one file too many. Notify
// is a *bool so "absent" and "explicitly off" are different facts: the
// default is on.
type uiEntry struct {
	Theme  string `toml:"theme,omitempty"`
	Notify *bool  `toml:"notify,omitempty"`
}

// updatesEntry holds the update settings. Booleans are *bool so "absent" and
// "explicitly off" are different facts: the defaults are on.
type updatesEntry struct {
	Check   *bool  `toml:"check,omitempty"`
	Auto    *bool  `toml:"auto,omitempty"`
	Channel string `toml:"channel,omitempty"`
}

// providerEntry redirects a provider at a different endpoint. A gateway, an
// Azure deployment, a self-hosted proxy, and a corporate egress point all need
// this, and hardcoding one address per vendor would make every one of them a
// code change.
//
// It does not change target identity. A provider reached at another address is
// still that provider as far as the catalog and the credential are concerned,
// so redirecting to something that prices differently is the user asserting
// they know that.
type providerEntry struct {
	BaseURL string `toml:"base_url"`
}

// authEntry configures where a provider's credential comes from. It carries no
// field for the credential itself: §5.3 keeps secrets out of this file, and a
// key that exists is a key someone pastes a secret into.
type authEntry struct {
	Env    string      `toml:"env,omitempty"`
	Helper []string    `toml:"helper,omitempty"`
	OAuth  *oauthEntry `toml:"oauth,omitempty"`
}

// oauthEntry configures a login flow. It has a client id and no client secret,
// because a command-line program cannot keep one: this is a public client and
// PKCE is what stands in for a secret. A field for one would invite storing it
// here in the clear, which is the thing §5.3 rules out.
type oauthEntry struct {
	ClientID     string            `toml:"client_id"`
	AuthorizeURL string            `toml:"authorize_url"`
	TokenURL     string            `toml:"token_url"`
	Scopes       []string          `toml:"scopes,omitempty"`
	Audience     string            `toml:"audience,omitempty"`
	RedirectPort int               `toml:"redirect_port,omitempty"`
	ExtraParams  map[string]string `toml:"extra_params,omitempty"`
}

type tierEntry struct {
	Label    string   `toml:"label,omitempty"`
	Model    string   `toml:"model"`
	Surface  string   `toml:"surface,omitempty"`
	Effort   string   `toml:"effort,omitempty"`
	Fallback []string `toml:"fallback,omitempty"`
}

// Load reads the user's configuration. A missing file is not an error: the
// tool runs without one, driven entirely by flags.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return &Config{
			Slots:            map[string]string{},
			Auth:             map[string]credential.Settings{},
			Providers:        map[string]ProviderSettings{},
			UpdateCheck:      true,
			UpdateAuto:       true,
			CompactAuto:      true,
			CompactAtPercent: 85,
		}, nil
	}
	return LoadFile(path)
}

func LoadFile(path string) (*Config, error) {
	c := &Config{
		Slots:            map[string]string{},
		Auth:             map[string]credential.Settings{},
		Providers:        map[string]ProviderSettings{},
		UpdateCheck:      true,
		UpdateAuto:       true,
		CompactAuto:      true,
		CompactAtPercent: 85,
		Path:             path,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var f file
	meta, err := toml.Decode(string(data), &f)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		// A misspelled key that is silently ignored is a configuration the
		// user believes is in effect and is not.
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("%s: unrecognized settings: %s", path, strings.Join(keys, ", "))
	}

	for k, v := range f.Slots {
		c.Slots[k] = v
	}
	for k, v := range f.Auth {
		s := credential.Settings{Env: v.Env, Helper: v.Helper}
		if v.OAuth != nil {
			s.OAuth = credential.OAuthSettings{
				ClientID:        v.OAuth.ClientID,
				AuthorizeURL:    v.OAuth.AuthorizeURL,
				TokenURL:        v.OAuth.TokenURL,
				Scopes:          v.OAuth.Scopes,
				Audience:        v.OAuth.Audience,
				RedirectPort:    v.OAuth.RedirectPort,
				ExtraAuthParams: v.OAuth.ExtraParams,
			}
		}
		c.Auth[k] = s
	}
	for k, v := range f.Providers {
		c.Providers[k] = ProviderSettings{BaseURL: v.BaseURL}
	}
	if f.Updates.Check != nil {
		c.UpdateCheck = *f.Updates.Check
	}
	if f.Updates.Auto != nil {
		c.UpdateAuto = *f.Updates.Auto
	}
	switch f.Updates.Channel {
	case "", "stable", "beta":
		c.UpdateChannel = f.Updates.Channel
	default:
		// The §16/§18 posture on configuration mistakes: a value that is
		// silently ignored is a setting the user believes is in effect.
		return nil, fmt.Errorf("%s: updates.channel %q is not stable or beta", path, f.Updates.Channel)
	}
	if f.Compact.Auto != nil {
		c.CompactAuto = *f.Compact.Auto
	}
	if f.Compact.AtPercent != 0 {
		if f.Compact.AtPercent < 50 || f.Compact.AtPercent > 95 {
			// Below half the window it would compact constantly; above 95 it
			// would fire after the request that already failed.
			return nil, fmt.Errorf("%s: compact.at_percent %d is outside 50–95", path, f.Compact.AtPercent)
		}
		c.CompactAtPercent = f.Compact.AtPercent
	}
	c.Theme = f.UI.Theme
	c.Notify = f.UI.Notify
	if f.Limits.Budget < 0 {
		return nil, fmt.Errorf("%s: limits.budget %s is negative; a ceiling below zero rules out every turn", path, f.Limits.Budget)
	}
	c.Budget = f.Limits.Budget
	if err := c.buildTiers(f.Tiers, path); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) buildTiers(entries map[string]tierEntry, path string) error {
	if len(entries) == 0 {
		return nil
	}
	if len(entries) > MaxTiers {
		return fmt.Errorf("%s: %d tiers configured, more than the %d ceiling", path, len(entries), MaxTiers)
	}

	ids := make([]string, 0, len(entries))
	for id := range entries {
		if _, err := tierNumber(id); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		ids = append(ids, id)
	}
	// Numeric order, not lexical: t10 comes after t9.
	sort.Slice(ids, func(i, j int) bool {
		a, _ := tierNumber(ids[i])
		b, _ := tierNumber(ids[j])
		return a < b
	})

	for _, id := range ids {
		entry := entries[id]
		target, err := ParseTarget(entry.Model, entry.Surface, entry.Effort)
		if err != nil {
			return fmt.Errorf("%s: tier %s: %w", path, id, err)
		}
		tier := Tier{ID: id, Label: entry.Label, Target: target}
		for _, ref := range entry.Fallback {
			fb, err := ParseTarget(ref, "", "")
			if err != nil {
				return fmt.Errorf("%s: tier %s fallback: %w", path, id, err)
			}
			tier.Fallbacks = append(tier.Fallbacks, fb)
		}
		c.Tiers = append(c.Tiers, tier)
	}
	return nil
}

// tierNumber enforces the t1..tN scheme. Numeric IDs are the only scheme that
// generalizes over a configurable N without encoding a capability claim the
// system cannot guarantee (§3.1).
func tierNumber(id string) (int, error) {
	rest, ok := strings.CutPrefix(id, "t")
	if !ok {
		return 0, fmt.Errorf("tier %q must be named t1 through t%d", id, MaxTiers)
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("tier %q must be named t1 through t%d", id, MaxTiers)
	}
	return n, nil
}

// defaultSurfaces records the serving surface assumed when configuration names
// only a provider and a model. Anything else has to be written out, because
// price, cache behavior, and retention differ per surface and guessing one
// would attach the wrong catalog entry.
var defaultSurfaces = map[string]string{
	"ollama":    "local",
	"anthropic": "first-party",
	"openai":    "first-party",
	"kimi":      "coding",
}

// ParseTarget reads a "provider/model" reference into a route target.
//
// The split is on the first slash only, because model identifiers legitimately
// contain slashes: an Ollama model pulled from a registry is named like
// "hf.co/user/model".
func ParseTarget(ref, surface, effort string) (provider.RouteTarget, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return provider.RouteTarget{}, errors.New("no model given")
	}

	providerName, model, ok := strings.Cut(ref, "/")
	if !ok || model == "" {
		return provider.RouteTarget{}, fmt.Errorf(
			"model %q must be written as provider/model, for example ollama/qwen3.5:9b-mlx", ref)
	}

	if surface == "" {
		known, ok := defaultSurfaces[providerName]
		if !ok {
			return provider.RouteTarget{}, fmt.Errorf(
				"provider %q has no default serving surface; set surface explicitly", providerName)
		}
		surface = known
	}

	target := provider.RouteTarget{Provider: providerName, Surface: surface, ModelID: model}
	if effort != "" {
		target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: effort}
	}
	return target, nil
}

func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".switchboard", FileName), nil
}
