package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

var ocrDownloadClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{DialContext: dialOCRDocument, IdleConnTimeout: 30 * time.Second},
	CheckRedirect: func(request *http.Request, previous []*http.Request) error {
		if len(previous) >= 5 || !validOCRDocumentRequest(request) {
			return errors.New("invalid document redirect")
		}
		return nil
	},
}

func validOCRDocumentRequest(request *http.Request) bool {
	address := request.URL
	return (address.Scheme == "http" || address.Scheme == "https") && address.Hostname() != "" && address.User == nil
}

var ocrRestrictedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("2002::/16"),
}

func dialOCRDocument(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 4 * time.Second}
	for _, ip := range ips {
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Zone() != "" {
			continue
		}
		restricted := false
		for _, prefix := range ocrRestrictedNetworks {
			restricted = restricted || prefix.Contains(ip)
		}
		if !restricted {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
		}
	}
	return nil, errors.New("no reachable public document address")
}

func preparePartnerOCR(ctx context.Context, fields map[string]any, limit int64) (map[string]any, error) {
	document, _ := fields["document"].(map[string]any)
	kind, _ := document["type"].(string)
	address, _ := document[kind].(string)
	if (kind != "document_url" && kind != "image_url") || address == "" || strings.HasPrefix(address, "data:") {
		return fields, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil || !validOCRDocumentRequest(request) {
		return nil, errors.New("invalid OCR document URL")
	}
	response, err := ocrDownloadClient.Do(request)
	if err != nil {
		return nil, errors.New("could not download OCR document")
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 || response.ContentLength > limit {
		return nil, errors.New("OCR document request failed or exceeded the size limit")
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(content)) > limit || len(content) == 0 {
		return nil, errors.New("invalid or oversized OCR document")
	}
	media, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if media == "" || media == "application/octet-stream" {
		media, _, _ = mime.ParseMediaType(http.DetectContentType(content))
	}
	prepared := clone(fields)
	document = clone(document)
	document[kind] = "data:" + media + ";base64," + base64.StdEncoding.EncodeToString(content)
	prepared["document"] = document
	return prepared, nil
}
