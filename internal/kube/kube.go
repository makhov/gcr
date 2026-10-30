// Package kube is the little of Kubernetes' API gcr's installer needs, from
// inside a pod: labelling its node.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
)

// serviceAccount is where a pod finds its credentials.
const serviceAccount = "/var/run/secrets/kubernetes.io/serviceaccount"

// LabelNode sets label key on node to value, or with set false removes it,
// as the pod's service account.
func LabelNode(ctx context.Context, node, key, value string, set bool) error {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return fmt.Errorf("not in a pod: no KUBERNETES_SERVICE_HOST")
	}
	token, err := os.ReadFile(serviceAccount + "/token")
	if err != nil {
		return err
	}
	ca, err := os.ReadFile(serviceAccount + "/ca.crt")
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return fmt.Errorf("no certificate in %s/ca.crt", serviceAccount)
	}
	var v any = value
	if !set {
		v = nil // a merge patch removes a key set to null
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]any{key: v}}})
	if err != nil {
		return err
	}
	u := "https://" + net.JoinHostPort(host, port) + "/api/v1/nodes/" + node
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("label node %s: %s: %s", node, resp.Status, bytes.TrimSpace(b))
	}
	return nil
}
