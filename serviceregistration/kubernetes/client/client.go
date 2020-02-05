package client

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-hclog"
)

// maxRetries is the maximum number of times the client
// should retry.
const maxRetries = 10

var (
	ErrNamespaceUnset = errors.New(`"namespace" is unset`)
	ErrPodNameUnset   = errors.New(`"podName" is unset`)
	ErrNotInCluster   = errors.New("unable to load in-cluster configuration, KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT must be defined")
)

// New instantiates a Client. The stopCh is used for exiting retry loops
// when closed.
func New(logger hclog.Logger, stopCh <-chan struct{}) (*Client, error) {
	config, err := inClusterConfig()
	if err != nil {
		return nil, err
	}
	return &Client{
		logger: logger,
		config: config,
		stopCh: stopCh,
	}, nil
}

// Client is a minimal Kubernetes client. We rolled our own because the existing
// Kubernetes client-go library available externally has a high number of dependencies
// and we thought it wasn't worth it for only two API calls. If at some point they break
// the client into smaller modules, or if we add quite a few methods to this client, it may
// be worthwhile to revisit that decision.
type Client struct {
	logger hclog.Logger
	config *Config
	stopCh <-chan struct{}
}

// GetPod gets a pod from the Kubernetes API.
func (c *Client) GetPod(namespace, podName string) (*Pod, error) {
	endpoint := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", namespace, podName)
	method := http.MethodGet

	// Validate that we received required parameters.
	if namespace == "" {
		return nil, ErrNamespaceUnset
	}
	if podName == "" {
		return nil, ErrPodNameUnset
	}

	req, err := http.NewRequest(method, c.config.Host+endpoint, nil)
	if err != nil {
		return nil, err
	}
	pod := &Pod{}
	if err := c.do(req, pod); err != nil {
		return nil, err
	}
	return pod, nil
}

// PatchPod updates the pod's tags to the given ones.
// It does so non-destructively, or in other words, without tearing down
// the pod.
func (c *Client) PatchPod(namespace, podName string, patches ...*Patch) error {
	endpoint := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", namespace, podName)
	method := http.MethodPatch

	// Validate that we received required parameters.
	if namespace == "" {
		return ErrNamespaceUnset
	}
	if podName == "" {
		return ErrPodNameUnset
	}
	if len(patches) == 0 {
		// No work to perform.
		return nil
	}

	var jsonPatches []map[string]interface{}
	for _, patch := range patches {
		if patch.Operation == Unset {
			return errors.New("patch operation must be set")
		}
		jsonPatches = append(jsonPatches, map[string]interface{}{
			"op":    patch.Operation,
			"path":  patch.Path,
			"value": patch.Value,
		})
	}
	body, err := json.Marshal(jsonPatches)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(method, c.config.Host+endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json-patch+json")
	return c.do(req, nil)
}

// do executes the given request, retrying if necessary.
func (c *Client) do(req *http.Request, ptrToReturnObj interface{}) error {
	// Finish setting up a valid request.
	req.Header.Set("Authorization", "Bearer "+c.config.BearerToken)
	req.Header.Set("Accept", "application/json")
	client := cleanhttp.DefaultClient()
	client.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: c.config.CACertPool,
		},
	}

	// Execute and retry the request. This exponential backoff comes
	// with jitter already rolled in.
	var lastErr error
	b := backoff.NewExponentialBackOff()
	for i := 0; i < maxRetries; i++ {
		if i != 0 {
			select {
			case <-c.stopCh:
				return nil
			case <-time.NewTimer(b.NextBackOff()).C:
				// Continue to the request.
			}
		}
		shouldRetry, err := c.attemptRequest(client, req, ptrToReturnObj)
		if !shouldRetry {
			// The error may be nil or populated depending on whether the
			// request was successful.
			return err
		}
		lastErr = err
	}
	return lastErr
}

// attemptRequest tries one single request. It's in its own function so each
// response body can be closed before returning, which would read awkwardly if
// executed in a loop.
func (c *Client) attemptRequest(client *http.Client, req *http.Request, ptrToReturnObj interface{}) (shouldRetry bool, err error) {
	// Preserve the original request body so it can be viewed for debugging if needed.
	// Reading it empties it, so we need to re-add it afterwards.
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = ioutil.ReadAll(req.Body)
		reqBodyReader := bytes.NewReader(reqBody)
		req.Body = ioutil.NopCloser(reqBodyReader)
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			if c.logger.IsWarn() {
				// Failing to close response bodies can present as a memory leak so it's
				// important to surface it.
				c.logger.Warn(fmt.Sprintf("unable to close response body: %s", err))
			}
		}
	}()

	// Check for success.
	switch resp.StatusCode {
	case 200, 201, 202, 204:
		// Pass.
	case 401, 403:
		// Perhaps the token from our bearer token file has been refreshed.
		config, err := inClusterConfig()
		if err != nil {
			return false, err
		}
		if config.BearerToken == c.config.BearerToken {
			// It's the same token.
			return false, fmt.Errorf("bad status code: %s", sanitizedDebuggingInfo(req, reqBody, resp))
		}
		c.config = config
		// Continue to try again, but return the error too in case the caller would rather read it out.
		return true, fmt.Errorf("bad status code: %s", sanitizedDebuggingInfo(req, reqBody, resp))
	case 404:
		return false, &ErrNotFound{debuggingInfo: sanitizedDebuggingInfo(req, reqBody, resp)}
	case 500, 502, 503, 504:
		// Could be transient.
		return true, fmt.Errorf("unexpected status code: %s", sanitizedDebuggingInfo(req, reqBody, resp))
	default:
		// Unexpected.
		return false, fmt.Errorf("unexpected status code: %s", sanitizedDebuggingInfo(req, reqBody, resp))
	}

	// We only arrive here with success.
	// If we're not supposed to read out the body, we have nothing further
	// to do here.
	if ptrToReturnObj == nil {
		return false, nil
	}

	// Attempt to read out the body into the given return object.
	return false, json.NewDecoder(resp.Body).Decode(ptrToReturnObj)
}

type Pod struct {
	Metadata *Metadata `json:"metadata,omitempty"`
}

type Metadata struct {
	Name string `json:"name,omitempty"`

	// This map will be nil if no "labels" key was provided.
	// It will be populated but have a length of zero if the
	// key was provided, but no values.
	Labels map[string]string `json:"labels,omitempty"`
}

type PatchOperation string

const (
	Unset   PatchOperation = "unset"
	Add                    = "add"
	Replace                = "replace"
)

type Patch struct {
	Operation PatchOperation
	Path      string
	Value     interface{}
}

type ErrNotFound struct {
	debuggingInfo string
}

func (e *ErrNotFound) Error() string {
	return e.debuggingInfo
}

// sanitizedDebuggingInfo converts an http response to a string without
// including its headers to avoid leaking authorization headers.
func sanitizedDebuggingInfo(req *http.Request, reqBody []byte, resp *http.Response) string {
	respBody, _ := ioutil.ReadAll(resp.Body)
	return fmt.Sprintf("req method: %s, req url: %s, req body: %s, resp statuscode: %d, resp respBody: %s", req.Method, req.URL, reqBody, resp.StatusCode, respBody)
}
