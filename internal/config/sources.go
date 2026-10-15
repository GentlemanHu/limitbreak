package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"quota-session-orchestrator/internal/domain"
)

// Source is a closed declarative contract. No inline credentials, file discovery,
// dynamic code, custom headers, shell commands or plugin loading are accepted.
type Source struct {
	ID                 string       `json:"id"`
	Type               string       `json:"type"`
	URL                string       `json:"url"`
	CredentialEnv      string       `json:"credential_env,omitempty"`
	ManagementPrefix   string       `json:"management_prefix,omitempty"`
	OnlyEmail          string       `json:"only_email,omitempty"`
	PreferPlus         *bool        `json:"prefer_plus,omitempty"`
	PollSeconds        int          `json:"poll_seconds,omitempty"`
	HTTPTimeoutSeconds int          `json:"http_timeout_seconds,omitempty"`
	MaxBodyBytes       int64        `json:"max_body_bytes,omitempty"`
	Schema             string       `json:"schema,omitempty"`
	Queries            []QuotaQuery `json:"queries,omitempty"`
}

func (s *Source) UnmarshalJSON(data []byte) error {
	type plain Source
	var value plain
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	*s = Source(value)
	return nil
}

var sourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

func ValidateCredentialEnv(ref string) error {
	if ref == "" {
		return nil
	}
	if !envPattern.MatchString(ref) {
		return errors.New("invalid explicit credential_env reference")
	}
	upper := strings.ToUpper(ref)
	for _, prefix := range []string{"DYLD_", "LD_", "LC_", "CGO_", "PYTHON", "NODE_", "RUBY", "PERL", "XDG_", "SWIFT_", "OBJC_", "BASH_", "JAVA_", "JDK_", "DOTNET_", "COMPLUS_"} {
		if strings.HasPrefix(upper, prefix) {
			return errors.New("credential_env cannot name a system/runtime control variable")
		}
	}
	switch upper {
	case "PATH", "HOME", "TMPDIR", "TMP", "TEMP", "SHELL", "IFS", "ENV", "BASH_ENV", "ZDOTDIR", "USER", "LOGNAME", "PWD", "OLDPWD", "CDPATH", "LANG", "TZ",
		"GOENV", "GOROOT", "GOPATH", "GOFLAGS", "GOTOOLCHAIN", "GOMODCACHE", "GOCACHE", "GODEBUG", "GOTRACEBACK", "GOMAXPROCS", "GOMEMLIMIT", "GOTMPDIR", "GOWORK", "GOPROXY", "GOSUMDB", "GONOPROXY", "GONOSUMDB", "GOPRIVATE", "GOAUTH", "GOBIN", "GOOS", "GOARCH", "GOEXPERIMENT", "GO111MODULE", "GOTOOLDIR", "_JAVA_OPTIONS", "SSH_ASKPASS", "SSH_AUTH_SOCK", "DISPLAY", "WAYLAND_DISPLAY", "COMSPEC", "SYSTEMROOT", "APPDATA", "LOCALAPPDATA",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_OPTIONS":
		return errors.New("credential_env cannot name a system/runtime control variable")
	}
	return nil
}

// ValidateSourceURL prevents implicit origins and URL-embedded secrets. HTTP is
// supported only with a literal loopback IP (localhost is not DNS-resolved here).
func ValidateSourceURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "\r\n\x00") {
		return errors.New("source URL requires an explicit origin without credentials, query or fragment")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("source URL must use HTTPS or literal loopback HTTP")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return errors.New("HTTP source must use a literal loopback IP")
		}
	}
	if u.Hostname() == "" {
		return errors.New("source hostname is required")
	}
	return nil
}

func (c Config) EffectiveSources() []Source {
	if len(c.Sources) != 0 {
		return append([]Source(nil), c.Sources...)
	}
	prefer := c.PreferPlus
	return []Source{{ID: domain.DefaultSourceID, Type: "cliproxyapi", URL: c.ProxyURL,
		CredentialEnv: c.ManagementKeyEnv, ManagementPrefix: c.ManagementPrefix,
		OnlyEmail: c.OnlyEmail, PreferPlus: &prefer, PollSeconds: c.PollSeconds,
		HTTPTimeoutSeconds: c.HTTPTimeoutSeconds, Queries: c.Queries}}
}

func (c Config) validateSources() error {
	if len(c.Sources) > 16 {
		return errors.New("at most 16 quota sources are supported")
	}
	seen := map[string]bool{}
	credentials := map[string]bool{}
	for _, s := range c.Sources {
		if !sourceIDPattern.MatchString(s.ID) || seen[s.ID] {
			return errors.New("source IDs must be unique 1–64 character identifiers")
		}
		seen[s.ID] = true
		if err := ValidateSourceURL(s.URL); err != nil {
			return fmt.Errorf("source %s: %w", s.ID, err)
		}
		if err := ValidateCredentialEnv(s.CredentialEnv); err != nil {
			return fmt.Errorf("source %s: %w", s.ID, err)
		}
		if s.CredentialEnv != "" {
			if credentials[s.CredentialEnv] {
				return errors.New("different sources cannot share credential_env; use one explicit environment reference per identity")
			}
			credentials[s.CredentialEnv] = true
		}
		if s.PollSeconds != 0 && (s.PollSeconds < 30 || s.PollSeconds > 86400) {
			return errors.New("source poll_seconds must be 30–86400")
		}
		if s.HTTPTimeoutSeconds < 0 || s.HTTPTimeoutSeconds > 30 {
			return errors.New("source HTTP timeout must be 1–30 seconds or omitted")
		}
		switch s.Type {
		case "cliproxyapi":
			if s.CredentialEnv == "" {
				return errors.New("explicit CPA sources require credential_env; no secret discovery is performed")
			}
			if s.ManagementPrefix != "" && s.ManagementPrefix != "auto" && s.ManagementPrefix != "/v0/management" && s.ManagementPrefix != "/v8/management" {
				return errors.New("unsupported source management_prefix")
			}
			if s.Schema != "" || s.MaxBodyBytes != 0 {
				return errors.New("CPA sources do not accept readonly-http schema/body settings")
			}
			// Reuse the established quota query validation, without recursing into sources.
			copy := c
			copy.Sources = nil
			copy.Queries = s.Queries
			if err := copy.Validate(); err != nil {
				return err
			}
		case "readonly-http":
			if s.ID == domain.DefaultSourceID {
				return errors.New("cpa-default is reserved for cliproxyapi legacy identity")
			}
			if s.Schema != "normalized-v1" {
				return errors.New("readonly-http requires schema normalized-v1")
			}
			if s.ManagementPrefix != "" || s.OnlyEmail != "" || s.PreferPlus != nil || len(s.Queries) != 0 {
				return errors.New("readonly-http does not accept CPA settings")
			}
			if s.MaxBodyBytes < 0 || s.MaxBodyBytes > 4<<20 {
				return errors.New("source max_body_bytes must be 1–4194304 or omitted")
			}
		default:
			return fmt.Errorf("unsupported source type %q (no standalone product integration)", s.Type)
		}
	}
	return nil
}
