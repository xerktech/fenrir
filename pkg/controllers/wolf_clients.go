package controllers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/uuid"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// Wolf gives every stream its paired client's ID (a hash of the client's
// cert) as session ID, so all of a pod's streams share one ID. A dead
// client's ENet peer outlives its stream until Wolf times it out (seconds);
// the xerktech/wolf fork (XERK-1948) makes the newest peer own the ID, so
// that late timeout ends only the superseded stream, not a /resume's
// (EndSupersededStreamEvent). Earlier Wolf images paused every stream with
// the ID, which is why pods once rotated attaches over several clients
// (XERK-1380); one client is enough now (XERK-1964).
const (
	wolfStreamClients = 1
	// The clients' app_state_folder, plus their index; how the operator
	// finds their IDs, which Wolf computes.
	wolfStreamClientPrefix = "direwolf-stream-"
	// Wolf's config, in the wolf-data volume (HOST_APPS_STATE_FOLDER/cfg).
	wolfConfigPath = "/mnt/data/wolf/cfg/config.toml"
	// Env var carrying wolfConfigSeed into the init container.
	wolfConfigSeedEnv = "WOLF_CONFIG_SEED"
)

// wolfConfigSeedScript installs the seed unless Wolf's config already has
// the stream client. Volumes seeded with the old rotation's four clients
// also have direwolf-stream-0 and are kept. The config lives on the per user and App PVC, so this
// happens once per volume; a config without them is kept beside it.
var wolfConfigSeedScript = fmt.Sprintf(`
				cfg=%[1]s
				if ! grep -qs '%[2]s%[3]d' "$cfg"; then
					if [ -f "$cfg" ]; then mv "$cfg" "$cfg.pre-direwolf"; fi
					printf '%%s' "$%[4]s" > "$cfg"
				fi
`, wolfConfigPath, wolfStreamClientPrefix, wolfStreamClients-1, wolfConfigSeedEnv)

// wolfConfigSeed returns a config.toml holding only the stream client.
// Wolf can't start from a partial current config (it needs its encoder
// lists), so the seed is a version 6 config: Wolf migrates it, writing its
// full default config with the seed's hostname, uuid and paired_clients.
//
// Each client's cert is self-signed and its key discarded, so no one can
// present it to Wolf's HTTPS port.
func wolfConfigSeed() (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "hostname = \"Wolf\"\nuuid = %q\nconfig_version = 6\n", string(uuid.NewUUID()))
	for i := range wolfStreamClients {
		cert, err := generateStreamClientCert()
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "\n[[paired_clients]]\nclient_cert = '''\n%s'''\napp_state_folder = \"%s%d\"\n",
			cert, wolfStreamClientPrefix, i)
	}
	return b.String(), nil
}

// generateStreamClientCert makes a self-signed client cert for Wolf's
// paired_clients; its key is thrown away.
func generateStreamClientCert() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate Wolf stream client key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate Wolf stream client cert serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "direwolf-stream"},
		NotBefore:    now.Add(-time.Hour),
		// Wolf compares the cert, not its dates; it outlives the volume.
		NotAfter:    now.Add(100 * 365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("failed to create Wolf stream client cert: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// wolfStreamClientID picks the paired client every attach streams with: the
// first stream client, so a volume seeded with the old rotation's clients
// uses direwolf-stream-0. "" (Wolf's certless dummy) when Wolf has none,
// e.g. a pod from before the config was seeded.
func wolfStreamClientID(ctx context.Context, wolfclient wolfapi.Client) (string, error) {
	clients, err := wolfclient.ListClients(ctx)
	if err != nil {
		return "", fmt.Errorf("listing Wolf's paired clients: %w", err)
	}
	clients = slices.DeleteFunc(clients, func(c wolfapi.PairedClient) bool {
		return !strings.HasPrefix(c.AppStateFolder, wolfStreamClientPrefix)
	})
	if len(clients) == 0 {
		return "", nil
	}
	return slices.MinFunc(clients, func(a, b wolfapi.PairedClient) int { return strings.Compare(a.AppStateFolder, b.AppStateFolder) }).ClientID, nil
}
