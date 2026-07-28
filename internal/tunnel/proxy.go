package tunnel

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func ProxySpecForLog(proxySpec string) string {
	spec := strings.TrimSpace(proxySpec)
	if spec == "" || strings.EqualFold(spec, "direct") {
		return "direct"
	}
	if strings.EqualFold(spec, "env") || strings.EqualFold(spec, "auto") {
		return spec
	}
	if !strings.Contains(spec, "://") {
		spec = "http://" + spec
	}
	proxyURL, err := url.Parse(spec)
	if err != nil || proxyURL.Host == "" {
		return proxySpec
	}
	return proxyURLForLog(proxyURL)
}

func resolveProxyURL(targetAddr, proxySpec string) (*url.URL, error) {
	spec := strings.TrimSpace(proxySpec)
	if strings.EqualFold(spec, "env") || strings.EqualFold(spec, "auto") {
		reqURL := &url.URL{Scheme: "https", Host: targetAddr}
		req := &http.Request{URL: reqURL}
		return http.ProxyFromEnvironment(req)
	}
	if !strings.Contains(spec, "://") {
		spec = "http://" + spec
	}
	proxyURL, err := url.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	proxyURL.Scheme = strings.ToLower(proxyURL.Scheme)
	if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
		return nil, fmt.Errorf("unsupported proxy scheme %q; use http or https", proxyURL.Scheme)
	}
	if proxyURL.Host == "" {
		return nil, errors.New("proxy host is required")
	}
	return proxyURL, nil
}

func proxyURLForLog(proxyURL *url.URL) string {
	if proxyURL == nil {
		return "direct"
	}
	if proxyURL.Scheme == "" {
		return proxyURL.Host
	}
	return proxyURL.Scheme + "://" + proxyURL.Host
}
