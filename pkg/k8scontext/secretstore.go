// -------------------------------------------------------------------------------------------
// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License. See License.txt in the project root for license information.
// --------------------------------------------------------------------------------------------

package k8scontext

import (
	"bytes"
	"io/ioutil"
	"os"
	"os/exec"
	"sync"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	recognizedSecretType = "kubernetes.io/tls"
	tlsKey               = "tls.key"
	tlsCrt               = "tls.crt"
)

// SecretsKeeper is the interface definition for secret store
type SecretsKeeper interface {
	GetPfxCertificate(secretKey string) []byte
	convertSecret(secretKey string, secret *v1.Secret) error
	delete(secretKey string)
}

// SecretsStore maintains a cache of the deployment secrets.
type SecretsStore struct {
	conversionSync sync.Mutex
	Cache          cache.ThreadSafeStore
}

// NewSecretStore creates a new SecretsKeeper object
func NewSecretStore() SecretsKeeper {
	return &SecretsStore{
		Cache: cache.NewThreadSafeStore(cache.Indexers{}, cache.Indices{}),
	}
}

// GetPfxCertificate returns the certificate for the given secret key.
func (s *SecretsStore) GetPfxCertificate(secretKey string) []byte {
	certInterface, exists := s.Cache.Get(secretKey)
	if exists {
		if cert, ok := certInterface.([]byte); ok {
			return cert
		}
	}
	return nil
}

func (s *SecretsStore) delete(secretKey string) {
	s.conversionSync.Lock()
	defer s.conversionSync.Unlock()

	s.Cache.Delete(secretKey)
}

func (s *SecretsStore) convertSecret(secretKey string, secret *v1.Secret) error {
	s.conversionSync.Lock()
	defer s.conversionSync.Unlock()

	// check if this is a secret with the correct type
	if secret.Type != recognizedSecretType {
		glog.Errorf("secret [%v] is not type kubernetes.io/tls", secretKey)
		return ErrorUnknownSecretType
	}

	if len(secret.Data[tlsKey]) == 0 || len(secret.Data[tlsCrt]) == 0 {
		glog.Errorf("secret [%v] is malformed, tls.key or tls.crt is not defined", secretKey)
		return ErrorMalformedSecret
	}

	tempfileCert, err := ioutil.TempFile("", "appgw-ingress-cert")
	if err != nil {
		glog.Error("unable to create temporary file for certificate conversion")
		return ErrorCreatingFile
	}
	defer os.Remove(tempfileCert.Name())

	tempfileKey, err := ioutil.TempFile("", "appgw-ingress-key")
	if err != nil {
		glog.Error("unable to create temporary file for certificate conversion")
		return ErrorCreatingFile
	}
	defer os.Remove(tempfileKey.Name())

	if err := writeFileDecode(secret.Data["tls.crt"], tempfileCert); err != nil {
		glog.Errorf("unable to write secret [%v].tls.crt to temporary file, error: %v", secretKey, err)
		return ErrorWritingToFile
	}

	if err := writeFileDecode(secret.Data["tls.key"], tempfileKey); err != nil {
		glog.Errorf("unable to write secret [%v].tls.key to temporary file, error: %v", secretKey, err)
		return ErrorWritingToFile
	}

	// both cert and key are in temp file now, call openssl
	var cout, cerr bytes.Buffer
	cmd := exec.Command("openssl", "pkcs12", "-export", "-in", tempfileCert.Name(), "-inkey", tempfileKey.Name(), "-password", "pass:msazure")
	cmd.Stderr = &cerr
	cmd.Stdout = &cout

	// if openssl exited with an error or the output is empty, report error
	if err := cmd.Run(); err != nil || len(cout.Bytes()) == 0 {
		glog.Errorf("unable to export using openssl, error=[%v], stderr=[%v]", err, cerr.String())
		return ErrorExportingWithOpenSSL
	}

	pfxCert := cout.Bytes()

	glog.V(1).Infof("converted secret [%v]", secretKey)
	// TODO i'm not sure if comparison against existing certificate can help
	// us optimize by eliminating some events
	_, exists := s.Cache.Get(secretKey)
	if exists {
		s.Cache.Update(secretKey, pfxCert)
	} else {
		s.Cache.Add(secretKey, pfxCert)
	}

	return nil
}

func writeFileDecode(data []byte, fileHandle *os.File) error {
	if _, err := fileHandle.Write(data); err != nil {
		return err
	}
	if err := fileHandle.Close(); err != nil {
		return err
	}
	return nil
}
