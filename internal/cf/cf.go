// Package cf is a minimal Cloudflare REST API client: just the calls Wmail needs.
package cf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var BaseURL = "https://api.cloudflare.com/client/v4"

type Client struct {
	Token string
	HTTP  *http.Client
}

func New(token string) *Client {
	return &Client{Token: token, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Error struct {
	Status int
	Errors []apiError
}

func (e *Error) Error() string {
	var msgs []string
	for _, x := range e.Errors {
		msgs = append(msgs, fmt.Sprintf("%d: %s", x.Code, x.Message))
	}
	return fmt.Sprintf("cloudflare API %d: %s", e.Status, strings.Join(msgs, "; "))
}

// Do sends a JSON request (body may be nil) and decodes the envelope's "result" into out.
func (c *Client) Do(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	return c.DoRaw(method, path, "application/json", r, out)
}

func (c *Client) DoRaw(method, path, contentType string, body io.Reader, out any) error {
	_, err := c.send(method, path, contentType, body, out)
	return err
}

// send performs the request and returns the envelope's result_info.cursor (used for paging).
func (c *Client) send(method, path, contentType string, body io.Reader, out any) (string, error) {
	req, err := http.NewRequest(method, BaseURL+path, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var env struct {
		Success    bool            `json:"success"`
		Errors     []apiError      `json:"errors"`
		Result     json.RawMessage `json:"result"`
		ResultInfo struct {
			Cursor string `json:"cursor"`
		} `json:"result_info"`
	}
	if json.Unmarshal(raw, &env) != nil || !env.Success || resp.StatusCode >= 300 {
		return "", &Error{Status: resp.StatusCode, Errors: env.Errors}
	}
	if out != nil && len(env.Result) > 0 {
		return env.ResultInfo.Cursor, json.Unmarshal(env.Result, out)
	}
	return env.ResultInfo.Cursor, nil
}

/* ---------- Workers KV (fallback queue for inbound mail) ---------- */

type KVKey struct {
	Name     string            `json:"name"`
	Metadata map[string]string `json:"metadata"`
}

// KVList pages through every key, so records stuck at the front cannot hide newer ones.
func (c *Client) KVList(account, ns string) ([]KVKey, error) {
	var all []KVKey
	cursor := ""
	for {
		p := fmt.Sprintf("/accounts/%s/storage/kv/namespaces/%s/keys?limit=1000", account, ns)
		if cursor != "" {
			p += "&cursor=" + url.QueryEscape(cursor)
		}
		var page []KVKey
		next, err := c.send("GET", p, "", nil, &page)
		if err != nil {
			return all, err
		}
		all = append(all, page...)
		if next == "" || len(page) == 0 {
			return all, nil
		}
		cursor = next
	}
}

// KVGet returns the raw value; this endpoint is not wrapped in the JSON envelope.
func (c *Client) KVGet(account, ns, key string) ([]byte, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/accounts/%s/storage/kv/namespaces/%s/values/%s", BaseURL, account, ns, url.PathEscape(key)), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, &Error{Status: resp.StatusCode}
	}
	return io.ReadAll(resp.Body)
}

func (c *Client) KVDelete(account, ns, key string) error {
	return c.Do("DELETE", fmt.Sprintf("/accounts/%s/storage/kv/namespaces/%s/values/%s", account, ns, url.PathEscape(key)), nil, nil)
}
