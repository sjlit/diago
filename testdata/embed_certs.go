// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package testdata

import (
	"crypto/tls"
	_ "embed"
)

var (
	//go:embed certs/server.crt
	serverCRT []byte

	//go:embed certs/server.key
	serverKEY []byte

	//go:embed certs/client.crt
	clientCRT []byte

	//go:embed certs/client.key
	clientKEY []byte
)

func ServerCertificate() tls.Certificate {
	cert, err := tls.X509KeyPair(serverCRT, serverKEY)
	if err != nil {
		panic(err)
	}
	return cert
}

func ClientCertificate() tls.Certificate {
	cert, err := tls.X509KeyPair(clientCRT, clientKEY)
	if err != nil {
		panic(err)
	}
	return cert
}
