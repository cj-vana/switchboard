package credential

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// OAuthSettings configures an authorization-code flow with PKCE.
//
// Every field is data the user supplies. Switchboard registers no OAuth client
// of its own and ships no client ID, which is deliberate: presenting this
// program to an authorization server as some other program's registered client
// is a decision about identity, and it belongs to whoever runs it rather than
// to a constant compiled into a binary. §5.3 allows a published flow and
// nothing more, so the flow is here and the registration is not.
type OAuthSettings struct {
	ClientID     string
	AuthorizeURL string
	TokenURL     string
	Scopes       []string

	// Audience and ExtraAuthParams cover the parameters providers add beyond
	// the specification. They are passed through rather than enumerated,
	// because guessing which ones a given server wants is how this ends up with
	// a per-vendor branch for something that is configuration.
	Audience        string
	ExtraAuthParams map[string]string

	// RedirectPort pins the loopback port when a provider requires the
	// redirect URI to match a registration exactly. Zero picks a free one,
	// which RFC 8252 permits and which avoids a collision with whatever else
	// is listening.
	RedirectPort int
}

func (s OAuthSettings) configured() bool {
	return s.ClientID != "" && s.AuthorizeURL != "" && s.TokenURL != ""
}

// oauthAccount suffixes the reference so a token document and an API key for
// the same provider do not overwrite one another. It is visible in the
// credential store, which is the point: a user auditing their keychain should
// be able to tell which item is which.
func oauthAccount(ref Ref) Ref {
	ref.Account = account(ref) + "#oauth"
	return ref
}

// tokenSet is what gets stored. It is a document rather than a bare string
// because a refresh token and an expiry have to survive alongside the access
// token, and losing the refresh token turns a silent renewal into a login
// prompt in the middle of a turn.
type tokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// String redacts, for the same reason Secret does: this struct holds two
// credentials and passes through error paths.
func (t tokenSet) String() string   { return redacted }
func (t tokenSet) GoString() string { return redacted }

// expiryMargin renews early. A token that expires between the check and the
// request arrives at the server already dead, and the turn fails for a reason
// that has nothing to do with what the user asked.
const expiryMargin = 60 * time.Second

func (t tokenSet) expired(now time.Time) bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return now.Add(expiryMargin).After(t.ExpiresAt)
}

// OAuthStore supplies an access token, refreshing it when it has aged out.
//
// It reads and writes through the platform credential store rather than keeping
// its own file, so an OAuth token is protected exactly as well as an API key is
// and §5.3's rule against a plaintext fallback covers both.
type OAuthStore struct {
	Settings OAuthSettings

	// Store holds the token document. Nil uses the platform store.
	Store Writer

	// Now and HTTP exist so the refresh path can be tested without a clock or a
	// network.
	Now  func() time.Time
	HTTP *http.Client
}

func (s *OAuthStore) Name() string { return "OAuth" }

func (s *OAuthStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *OAuthStore) store() Writer {
	if s.Store != nil {
		return s.Store
	}
	return NewOSStore()
}

func (s *OAuthStore) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (s *OAuthStore) Get(ctx context.Context, ref Ref) (Secret, error) {
	if !s.Settings.configured() {
		return Secret{}, ErrNotFound
	}

	tokens, err := s.read(ctx, ref)
	if err != nil {
		return Secret{}, err
	}
	if !tokens.expired(s.now()) {
		return New(tokens.AccessToken, SourceOAuth, "stored token"), nil
	}

	if tokens.RefreshToken == "" {
		return Secret{}, fmt.Errorf(
			"the stored token for %s has expired and carries no refresh token; run: sb auth oauth login %s", ref, ref)
	}
	refreshed, err := s.refresh(ctx, tokens.RefreshToken)
	if err != nil {
		return Secret{}, err
	}
	if refreshed.RefreshToken == "" {
		// Some servers rotate the refresh token and some do not. Keeping the
		// old one when none is returned is the difference between a session
		// that renews indefinitely and one that has to be logged in again.
		refreshed.RefreshToken = tokens.RefreshToken
	}
	if err := s.write(ctx, ref, refreshed); err != nil {
		return Secret{}, err
	}
	return New(refreshed.AccessToken, SourceOAuth, "refreshed token"), nil
}

func (s *OAuthStore) read(ctx context.Context, ref Ref) (tokenSet, error) {
	secret, err := s.store().Get(ctx, oauthAccount(ref))
	if err != nil {
		return tokenSet{}, err
	}
	var tokens tokenSet
	if err := json.Unmarshal([]byte(secret.Expose()), &tokens); err != nil {
		return tokenSet{}, fmt.Errorf("the stored token document for %s is unreadable; run: sb auth oauth login %s", ref, ref)
	}
	if tokens.AccessToken == "" {
		return tokenSet{}, ErrNotFound
	}
	return tokens, nil
}

func (s *OAuthStore) write(ctx context.Context, ref Ref, tokens tokenSet) error {
	// Compact, because the platform store takes a single line and a document
	// with newlines in it would be stored truncated.
	body, err := json.Marshal(tokens)
	if err != nil {
		return err
	}
	return s.store().Set(ctx, oauthAccount(ref), string(body))
}

func (s *OAuthStore) Logout(ctx context.Context, ref Ref) error {
	return s.store().Delete(ctx, oauthAccount(ref))
}

// Login runs the authorization-code flow with PKCE.
//
// PKCE is not optional here even though a loopback redirect is used: a public
// client has no secret to prove it is itself, and without the verifier any
// process that observes the authorization code can redeem it.
func (s *OAuthStore) Login(ctx context.Context, ref Ref, prompt func(url string)) error {
	if !s.Settings.configured() {
		return errors.New("no OAuth client is configured for this provider; " +
			"set client_id, authorize_url, and token_url under [auth.<provider>.oauth]")
	}

	verifier, challenge, err := pkce()
	if err != nil {
		return err
	}
	state, err := randomString(24)
	if err != nil {
		return err
	}

	// 127.0.0.1 rather than localhost: RFC 8252 calls for the literal address,
	// because localhost can resolve to an interface the flow did not intend.
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.Settings.RedirectPort))
	if err != nil {
		return fmt.Errorf("opening a loopback port for the redirect: %w", err)
	}
	defer listener.Close()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", listener.Addr().(*net.TCPAddr).Port)

	codes := make(chan string, 1)
	failures := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if desc := q.Get("error"); desc != "" {
			http.Error(w, "authorization failed", http.StatusBadRequest)
			failures <- fmt.Errorf("the authorization server refused: %s %s", desc, q.Get("error_description"))
			return
		}
		// The state check is what makes this flow safe to run on a loopback
		// port anything on the machine could have reached first.
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			failures <- errors.New("the redirect carried the wrong state, so it did not come from the request this flow started")
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "no code", http.StatusBadRequest)
			failures <- errors.New("the redirect carried no authorization code")
			return
		}
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "Signed in. You can close this tab and return to the terminal.")
		codes <- code
	})}
	go srv.Serve(listener)
	defer srv.Close()

	authURL := s.authorizeURL(redirectURI, state, challenge)
	if prompt != nil {
		prompt(authURL)
	}
	openBrowser(authURL)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-failures:
		return err
	case code := <-codes:
		tokens, err := s.exchange(ctx, code, verifier, redirectURI)
		if err != nil {
			return err
		}
		return s.write(ctx, ref, tokens)
	}
}

func (s *OAuthStore) authorizeURL(redirectURI, state, challenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", s.Settings.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if len(s.Settings.Scopes) > 0 {
		q.Set("scope", strings.Join(s.Settings.Scopes, " "))
	}
	if s.Settings.Audience != "" {
		q.Set("audience", s.Settings.Audience)
	}
	for k, v := range s.Settings.ExtraAuthParams {
		q.Set(k, v)
	}

	sep := "?"
	if strings.Contains(s.Settings.AuthorizeURL, "?") {
		sep = "&"
	}
	return s.Settings.AuthorizeURL + sep + q.Encode()
}

func (s *OAuthStore) exchange(ctx context.Context, code, verifier, redirectURI string) (tokenSet, error) {
	return s.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {s.Settings.ClientID},
		"code_verifier": {verifier},
	})
}

func (s *OAuthStore) refresh(ctx context.Context, refreshToken string) (tokenSet, error) {
	return s.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {s.Settings.ClientID},
	})
}

func (s *OAuthStore) token(ctx context.Context, form url.Values) (tokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Settings.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenSet{}, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")

	resp, err := s.client().Do(req)
	if err != nil {
		return tokenSet{}, err
	}
	defer resp.Body.Close()

	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`

		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return tokenSet{}, fmt.Errorf("the token endpoint returned something that is not JSON (http %d)", resp.StatusCode)
	}
	if body.Error != "" {
		// The server's own words, never the request: the form carries a code or
		// a refresh token and neither belongs in an error message.
		return tokenSet{}, fmt.Errorf("the token endpoint refused: %s %s", body.Error, body.ErrorDescription)
	}
	if resp.StatusCode >= 300 {
		return tokenSet{}, fmt.Errorf("the token endpoint returned http %d", resp.StatusCode)
	}
	if body.AccessToken == "" {
		return tokenSet{}, errors.New("the token endpoint returned no access token")
	}

	tokens := tokenSet{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		TokenType:    body.TokenType,
	}
	if body.ExpiresIn > 0 {
		tokens.ExpiresAt = s.now().Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	return tokens, nil
}

// pkce returns a verifier and its S256 challenge.
func pkce() (verifier, challenge string, err error) {
	verifier, err = randomString(64)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// openBrowser is best effort. The URL is printed either way, because a headless
// machine has no browser to open and the flow still has to be completable by
// pasting it somewhere that does.
func openBrowser(target string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "linux":
		cmd = "xdg-open"
	default:
		return
	}
	_ = exec.Command(cmd, target).Start()
}
