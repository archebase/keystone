// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

func newCloudTransportCredentials(useTLS bool, caFile string, serverName string) (credentials.TransportCredentials, error) {
	if !useTLS {
		return insecure.NewCredentials(), nil
	}

	// If no CA file is provided, fall back to the system cert pool.
	var roots *x509.CertPool
	if caFile == "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system cert pool: %w", err)
		}
		roots = pool
	} else {
		// #nosec G304 -- CA path is operator-controlled config, not end-user input
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read tls ca file %s: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if ok := pool.AppendCertsFromPEM(pem); !ok {
			return nil, fmt.Errorf("append ca certs from %s: no certificates found", caFile)
		}
		roots = pool
	}

	return credentials.NewTLS(&tls.Config{
		RootCAs:    roots,
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	}), nil
}

