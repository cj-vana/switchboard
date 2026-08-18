package main

// Browsing a serving surface.
//
// The catalog names a handful of models it has priced and a set of surfaces it
// knows the mechanics of. Those are different lists, and the picker used to
// offer only the first: a Kimi plan, a ChatGPT plan, and any OpenAI-compatible
// server were surfaces the catalog described and no menu could ever reach, so
// the model ladder could not be built for them without hand-editing the config.
//
// The fix is to treat a surface as a destination. Picking one asks its own
// server what it serves, because that is the only authority on it — plan model
// slugs are not the developer API's names, a compatible endpoint serves
// whatever it was started with — and falls back to typing the id when the
// server will not say. A surface whose address is not knowable in advance asks
// for the address first, and stores it where the next launch reads it.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/ollama"
	"github.com/switchboard-code/switchboard/internal/provider/openaicompat"
)

const (
	browsePrefix  = "\x00browse "
	typeModelID   = "\x00type-model"
	setAddressID  = "\x00set-address"
	genericCompat = "generic"
)

// modelLister is the discovery every adapter here happens to share. It is
// declared at the point of use rather than in the provider package because
// listing is a convenience for the picker, not part of what it takes to drive
// a turn: an adapter that cannot list is still a working target.
type modelLister interface {
	Models(ctx context.Context) ([]string, error)
}

// surfaceTarget is the route target that stands for a whole surface. It names
// no model, which is exactly the question being asked.
func surfaceTarget(providerName, surface string) provider.RouteTarget {
	return provider.RouteTarget{Provider: providerName, Surface: surface}
}

// surfaceNeedsAddress reports whether nothing but configuration can say where
// this surface is served. The compatible adapter is the whole case: it exists
// because the address is not knowable in advance, and its ollama profile is
// the one exception, deriving its address from the local server's.
func surfaceNeedsAddress(providerName, surface string) bool {
	return providerName == openaicompat.Name && surface != "ollama"
}

// surfaceTakesAddress reports whether offering to change the address is worth
// a row. Every provider can be redirected with base_url, but for a hosted API
// that is a gateway setting people write down once, while for a local or
// compatible server the address is the setting.
func surfaceTakesAddress(providerName string) bool {
	return providerName == openaicompat.Name || providerName == ollama.Name
}

// addressKey is the config key one surface's address is stored under. The
// local server keeps the provider-wide key, because it has exactly one
// address and -host and OLLAMA_HOST already mean that one.
func addressKey(providerName, surface string) string {
	if providerName == ollama.Name {
		return ollama.Name
	}
	return config.ProviderSurfaceKey(providerName, surface)
}

// browseSurfaceCmd opens one surface: its address if it has none yet, then its
// models. onChoice is what the caller does with a pick, which is the only
// thing /models and first-run setup disagree about.
func browseSurfaceCmd(reg *providers, cfg *config.Config, choice modelChoice, onChoice func(modelChoice) tea.Cmd) tea.Cmd {
	providerName, surface := choice.provider, choice.surface
	again := func() tea.Cmd { return browseSurfaceCmd(reg, cfg, choice, onChoice) }

	current := cfg.ProviderForTarget(providerName, surface).BaseURL
	if surfaceNeedsAddress(providerName, surface) && current == "" {
		return askAddressCmd(reg, cfg, providerName, surface, again)
	}

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		names, listErr := listSurfaceModels(ctx, reg, providerName, surface)
		bind := func(model string) modelChoice {
			return modelChoice{
				ref:          providerName + "/" + model,
				provider:     providerName,
				surface:      surface,
				desc:         choice.desc,
				effortLevels: choice.effortLevels,
			}
		}

		var items []pickerItem
		for _, name := range names {
			items = append(items, pickerItem{id: name, label: providerName + "/" + name, desc: choice.desc})
		}
		// Typing the id is always offered, never only as a fallback: a server
		// that lists nothing, an account whose entitlement has not propagated,
		// and a model published this morning are all cases where the list is
		// right and out of date.
		items = append(items, pickerItem{
			id:    typeModelID,
			label: "type a model id…",
			desc:  listNote(len(names), listErr),
		})
		if surfaceTakesAddress(providerName) {
			addr := cfg.ProviderForTarget(providerName, surface).BaseURL
			if providerName == ollama.Name {
				addr = reg.localServer().BaseURL()
			}
			items = append(items, pickerItem{
				id:    setAddressID,
				label: "change the server address…",
				desc:  "now " + orNone(addr),
			})
		}

		return pickerMsg{
			title: "models on " + providerName + "/" + surface,
			items: items,
			action: func(id string) tea.Cmd {
				switch id {
				case setAddressID:
					return askAddressCmd(reg, cfg, providerName, surface, again)
				case typeModelID:
					return func() tea.Msg {
						return textPromptMsg{
							title: "model id on " + providerName + "/" + surface,
							help:  "the id the server itself uses, copied exactly",
							submit: func(model string) tea.Cmd {
								return onChoice(bind(model))
							},
						}
					}
				}
				return onChoice(bind(id))
			},
		}
	}
}

// askAddressCmd takes a server address and stores it, then runs then. The
// write goes to the config the next launch reads, and the cached adapters are
// dropped so the address takes effect on the next request rather than the
// next process.
func askAddressCmd(reg *providers, cfg *config.Config, providerName, surface string, then func() tea.Cmd) tea.Cmd {
	key := addressKey(providerName, surface)
	current := cfg.ProviderForTarget(providerName, surface).BaseURL
	help := "the base url, including any /v1 the server serves under"
	if providerName == ollama.Name {
		current = reg.localServer().BaseURL()
		help = "the Ollama server's address, without /v1"
	}
	return func() tea.Msg {
		return textPromptMsg{
			title:   "server address for " + providerName + "/" + surface,
			help:    help,
			initial: current,
			submit: func(value string) tea.Cmd {
				cfg.SetProviderBaseURL(key, value)
				if err := cfg.Save(); err != nil {
					return noticeCmd("error", "saving the address failed: "+err.Error())
				}
				if providerName == ollama.Name {
					reg.adoptOllamaHost(value)
				} else {
					reg.reset()
				}
				return then()
			},
		}
	}
}

// listSurfaceModels asks the surface's own server what it serves. A failure is
// returned rather than swallowed because the reason — no address, no key, a
// server that is down — is what the caller shows next to the row that types an
// id by hand.
func listSurfaceModels(ctx context.Context, reg *providers, providerName, surface string) ([]string, error) {
	client, err := reg.get(ctx, surfaceTarget(providerName, surface))
	if err != nil {
		return nil, err
	}
	lister, ok := client.(modelLister)
	if !ok {
		return nil, fmt.Errorf("the %s adapter cannot list models", providerName)
	}
	names, err := lister.Models(ctx)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// listNote is the one line under the type-it-in row. It says why the list
// above is the length it is, because "no models" and "no key" send the user to
// different places.
func listNote(listed int, err error) string {
	if err == nil {
		if listed == 0 {
			return "the server listed no models"
		}
		return "if the id you want is not listed"
	}
	var notFound *credential.NotFoundError
	if errors.As(err, &notFound) {
		return "no credential yet; type the id and setup will ask for one"
	}
	var apiErr *provider.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403) {
		return "the server refused the credential; /login stores a new one"
	}
	return firstLine(err.Error())
}

func orNone(s string) string {
	if s == "" {
		return "not set"
	}
	return s
}

// choiceProvider is the provider a choice names. The field is authoritative
// when set; otherwise it comes from the reference, so a choice built the old
// way — a bare provider/model pair — still answers the question.
func choiceProvider(c modelChoice) string {
	if c.provider != "" {
		return c.provider
	}
	name, _, _ := strings.Cut(c.ref, "/")
	return name
}

// needsNoCredential reports whether the chosen target reaches a server that
// issues no keys. Asking anyway is not harmless: it is a prompt with no
// correct answer, standing between the user and the rung they just picked.
func needsNoCredential(c modelChoice) bool {
	switch choiceProvider(c) {
	case ollama.Name:
		return true
	case openaicompat.Name:
		// Only the profile that is by definition the local server. Anything
		// else behind this adapter may well want a bearer token, and a
		// prompt that can be skipped with an empty entry is the honest way
		// to not know.
		return c.surface == "ollama"
	}
	return false
}
