// Package registry answers whether an image tag exists in its OCI registry,
// for the web UI's PR-image check and the AuthentikDeployment admission
// webhook alike.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// TagPattern is the OCI distribution spec's tag grammar.
var TagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// repositoryPattern is the distribution spec's repository name grammar:
// lowercase path components separated by "/".
var repositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)

const (
	dockerHubHost     = "docker.io"
	dockerHubEndpoint = "https://registry-1.docker.io"
)

// manifestAccept lists every manifest media type a tag may resolve to - a
// multi-arch index for authentik's own images, but any of them counts.
const manifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// ParseImage splits an image reference without tag or digest (e.g.
// "ghcr.io/goauthentik/server", "nginx") into its registry host and
// repository, applying Docker Hub's defaults the way a container runtime
// does.
func ParseImage(image string) (host, repository string, err error) {
	if strings.ContainsAny(image, "@") || strings.Contains(image[strings.LastIndex(image, "/")+1:], ":") {
		return "", "", fmt.Errorf("image %q must not include a tag or digest - set the tag separately", image)
	}
	host, repository = dockerHubHost, image
	if first, rest, ok := strings.Cut(image, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		host, repository = first, rest
	}
	if host == dockerHubHost && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	if !repositoryPattern.MatchString(repository) {
		return "", "", fmt.Errorf("image %q is not a valid image reference", image)
	}
	return host, repository, nil
}

// Checker looks up image tags. The zero value talks HTTPS to each image's
// own registry with http.DefaultClient.
type Checker struct {
	HTTP *http.Client
	// Endpoints overrides the base URL used for a registry host (tests).
	Endpoints map[string]string
}

func (c *Checker) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Checker) endpoint(host string) string {
	if e, ok := c.Endpoints[host]; ok {
		return e
	}
	if host == dockerHubHost {
		return dockerHubEndpoint
	}
	return "https://" + host
}

// TagExists answers whether image:tag resolves to a manifest. Public images
// still need an anonymous bearer token, fetched from wherever the
// registry's WWW-Authenticate challenge points.
func (c *Checker) TagExists(ctx context.Context, image, tag string) (bool, error) {
	if !TagPattern.MatchString(tag) {
		return false, fmt.Errorf("invalid image tag %q", tag)
	}
	host, repository, err := ParseImage(image)
	if err != nil {
		return false, err
	}
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", c.endpoint(host), repository, tag)

	res, err := c.headManifest(ctx, manifestURL, "")
	if err != nil {
		return false, err
	}
	if res.StatusCode == http.StatusUnauthorized {
		token, err := c.token(ctx, res.Header.Get("WWW-Authenticate"), repository)
		if err != nil {
			return false, err
		}
		if res, err = c.headManifest(ctx, manifestURL, token); err != nil {
			return false, err
		}
	}
	switch res.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("registry manifest request failed: %s", res.Status)
	}
}

func (c *Checker) headManifest(ctx context.Context, manifestURL, token string) (*http.Response, error) {
	res, err := c.request(ctx, http.MethodHead, manifestURL, token)
	if err != nil {
		return nil, err
	}
	_ = res.Body.Close()
	return res, nil
}

// request sends a registry request; the caller closes the body.
func (c *Checker) request(ctx context.Context, method, target, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry request failed: %w", err)
	}
	return res, nil
}

// Label reads a label from the image config of image:tag (its first
// platform's, for a multi-arch index) - e.g. org.opencontainers.image.version,
// to tell which build a floating tag like "latest" points at. Empty if the
// image has no such label.
func (c *Checker) Label(ctx context.Context, image, tag, label string) (string, error) {
	if !TagPattern.MatchString(tag) {
		return "", fmt.Errorf("invalid image tag %q", tag)
	}
	host, repository, err := ParseImage(image)
	if err != nil {
		return "", err
	}
	base := fmt.Sprintf("%s/v2/%s", c.endpoint(host), repository)
	token := ""
	get := func(path string, out any) error {
		res, err := c.request(ctx, http.MethodGet, base+path, token)
		if err == nil && res.StatusCode == http.StatusUnauthorized && token == "" {
			_ = res.Body.Close()
			if token, err = c.token(ctx, res.Header.Get("WWW-Authenticate"), repository); err != nil {
				return err
			}
			res, err = c.request(ctx, http.MethodGet, base+path, token)
		}
		if err != nil {
			return err
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("registry request for %s:%s failed: %s", image, tag, res.Status)
		}
		return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
	}

	type manifest struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	var m manifest
	if err := get("/manifests/"+tag, &m); err != nil {
		return "", err
	}
	if len(m.Manifests) > 0 {
		digest := m.Manifests[0].Digest
		m = manifest{}
		if err := get("/manifests/"+digest, &m); err != nil {
			return "", err
		}
	}
	var cfg struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := get("/blobs/"+m.Config.Digest, &cfg); err != nil {
		return "", err
	}
	return cfg.Config.Labels[label], nil
}

// token answers a Bearer challenge (RFC 6750, as the distribution token
// auth spec uses it) with an anonymous pull token for repository.
func (c *Checker) token(ctx context.Context, challenge, repository string) (string, error) {
	params, ok := parseBearerChallenge(challenge)
	if !ok || params["realm"] == "" {
		return "", errors.New("registry requires authentication but sent no bearer challenge")
	}
	q := url.Values{"scope": {"repository:" + repository + ":pull"}}
	if service := params["service"]; service != "" {
		q.Set("service", service)
	}
	tokenURL, err := url.Parse(params["realm"])
	if err != nil {
		return "", fmt.Errorf("invalid registry token realm: %w", err)
	}
	tokenURL.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", err
	}
	res, err := c.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("registry token request failed: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token request failed: %s", res.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode registry token: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	return body.AccessToken, nil
}

// parseBearerChallenge parses `Bearer realm="...",service="...",scope="..."`.
func parseBearerChallenge(header string) (map[string]string, bool) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return nil, false
	}
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var value string
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				return nil, false
			}
			value, rest = after[1:end+1], after[end+2:]
		} else {
			value, rest, _ = strings.Cut(after, ",")
		}
		params[key] = value
		rest = strings.TrimLeft(strings.TrimSpace(rest), ",")
		rest = strings.TrimSpace(rest)
	}
	return params, true
}
