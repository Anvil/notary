package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"os"
	"testing"

	"github.com/docker/notary/signer"
	"github.com/docker/notary/signer/keydbstore"
	"github.com/docker/notary/tuf/data"
	"github.com/docker/notary/utils"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

const (
	Cert = "../../fixtures/notary-signer.crt"
	Key  = "../../fixtures/notary-signer.key"
	Root = "../../fixtures/root-ca.crt"
)

// initializes a viper object with test configuration
func configure(jsonConfig string) *viper.Viper {
	config := viper.New()
	config.SetConfigType("json")
	config.ReadConfig(bytes.NewBuffer([]byte(jsonConfig)))
	return config
}

// If the TLS configuration is invalid, an error is returned.  This doesn't test
// all the cases of the TLS configuration being invalid, since it's just
// calling configuration.ParseTLSConfig - this test just makes sure the
// error is propagated.
func TestGetAddrAndTLSConfigInvalidTLS(t *testing.T) {
	invalids := []string{
		`{"server": {"http_addr": ":1234", "grpc_addr": ":2345"}}`,
		`{"server": {
				"http_addr": ":1234",
				"grpc_addr": ":2345",
				"tls_cert_file": "nope",
				"tls_key_file": "nope"
		}}`,
	}
	for _, configJSON := range invalids {
		_, _, _, err := getAddrAndTLSConfig(configure(configJSON))
		require.Error(t, err)
		require.Contains(t, err.Error(), "unable to set up TLS")
	}
}

// If a GRPC address is not provided, an error is returned.
func TestGetAddrAndTLSConfigNoGRPCAddr(t *testing.T) {
	_, _, _, err := getAddrAndTLSConfig(configure(fmt.Sprintf(`{
		"server": {
			"http_addr": ":1234",
			"tls_cert_file": "%s",
			"tls_key_file": "%s"
		}
	}`, Cert, Key)))
	require.Error(t, err)
	require.Contains(t, err.Error(), "grpc listen address required for server")
}

// If an HTTP address is not provided, an error is returned.
func TestGetAddrAndTLSConfigNoHTTPAddr(t *testing.T) {
	_, _, _, err := getAddrAndTLSConfig(configure(fmt.Sprintf(`{
		"server": {
			"grpc_addr": ":1234",
			"tls_cert_file": "%s",
			"tls_key_file": "%s"
		}
	}`, Cert, Key)))
	require.Error(t, err)
	require.Contains(t, err.Error(), "http listen address required for server")
}

// Success parsing a valid TLS config, HTTP address, and GRPC address.
func TestGetAddrAndTLSConfigSuccess(t *testing.T) {
	httpAddr, grpcAddr, tlsConf, err := getAddrAndTLSConfig(configure(fmt.Sprintf(`{
		"server": {
			"http_addr": ":2345",
			"grpc_addr": ":1234",
			"tls_cert_file": "%s",
			"tls_key_file": "%s"
		}
	}`, Cert, Key)))
	require.NoError(t, err)
	require.Equal(t, ":2345", httpAddr)
	require.Equal(t, ":1234", grpcAddr)
	require.NotNil(t, tlsConf)
}

// If a default alias is not provided to a DB backend, an error is returned.
func TestSetupCryptoServicesDBStoreNoDefaultAlias(t *testing.T) {
	tmpFile, err := ioutil.TempFile("/tmp", "sqlite3")
	require.NoError(t, err)
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	_, err = setUpCryptoservices(
		configure(fmt.Sprintf(
			`{"storage": {"backend": "%s", "db_url": "%s"}}`,
			utils.SqliteBackend, tmpFile.Name())),
		[]string{utils.SqliteBackend})
	require.Error(t, err)
	require.Contains(t, err.Error(), "must provide a default alias for the key DB")
}

// If a default alias *is* provided to a valid DB backend, a valid
// CryptoService is returned.  (This depends on ParseStorage, which is tested
// separately, so this doesn't test all the possible cases of storage
// success/failure).
func TestSetupCryptoServicesDBStoreSuccess(t *testing.T) {
	tmpFile, err := ioutil.TempFile("/tmp", "sqlite3")
	require.NoError(t, err)
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	// Ensure that the private_key table exists
	db, err := gorm.Open("sqlite3", tmpFile.Name())
	require.NoError(t, err)
	var (
		gormKey = keydbstore.GormPrivateKey{}
		count   int
	)
	db.CreateTable(&gormKey)
	db.Model(&gormKey).Count(&count)
	require.Equal(t, 0, count)

	cryptoServices, err := setUpCryptoservices(
		configure(fmt.Sprintf(
			`{"storage": {"backend": "%s", "db_url": "%s"},
			"default_alias": "timestamp"}`,
			utils.SqliteBackend, tmpFile.Name())),
		[]string{utils.SqliteBackend})
	require.NoError(t, err)
	require.Len(t, cryptoServices, 2)

	edService, ok := cryptoServices[data.ED25519Key]
	require.True(t, ok)

	ecService, ok := cryptoServices[data.ECDSAKey]
	require.True(t, ok)

	require.Equal(t, edService, ecService)

	// since the keystores are not exposed by CryptoService, try creating
	// a key and seeing if it is in the sqlite DB.
	os.Setenv("NOTARY_SIGNER_TIMESTAMP", "password")
	defer os.Unsetenv("NOTARY_SIGNER_TIMESTAMP")

	_, err = ecService.Create("timestamp", "", data.ECDSAKey)
	require.NoError(t, err)
	db.Model(&gormKey).Count(&count)
	require.Equal(t, 1, count)
}

// If a memory backend is specified, then a default alias is not needed, and
// a valid CryptoService is returned.
func TestSetupCryptoServicesMemoryStore(t *testing.T) {
	config := configure(fmt.Sprintf(`{"storage": {"backend": "%s"}}`,
		utils.MemoryBackend))
	cryptoServices, err := setUpCryptoservices(config,
		[]string{utils.SqliteBackend, utils.MemoryBackend})
	require.NoError(t, err)
	require.Len(t, cryptoServices, 2)

	edService, ok := cryptoServices[data.ED25519Key]
	require.True(t, ok)

	ecService, ok := cryptoServices[data.ECDSAKey]
	require.True(t, ok)

	require.Equal(t, edService, ecService)

	// since the keystores are not exposed by CryptoService, try creating
	// and getting the key
	pubKey, err := ecService.Create("", "", data.ECDSAKey)
	require.NoError(t, err)
	privKey, _, err := ecService.GetPrivateKey(pubKey.ID())
	require.NoError(t, err)
	require.NotNil(t, privKey)
}

func TestSetupHTTPServer(t *testing.T) {
	httpServer := setupHTTPServer(":4443", nil, make(signer.CryptoServiceIndex))
	require.Equal(t, ":4443", httpServer.Addr)
	require.Nil(t, httpServer.TLSConfig)
}

func TestSetupGRPCServerInvalidAddress(t *testing.T) {
	_, _, err := setupGRPCServer("nope", nil, make(signer.CryptoServiceIndex))
	require.Error(t, err)
	require.Contains(t, err.Error(), "grpc server failed to listen on nope")
}

func TestSetupGRPCServerSuccess(t *testing.T) {
	tlsConf := tls.Config{InsecureSkipVerify: true}
	grpcServer, lis, err := setupGRPCServer(":7899", &tlsConf,
		make(signer.CryptoServiceIndex))
	defer lis.Close()
	require.NoError(t, err)
	require.Equal(t, "[::]:7899", lis.Addr().String())
	require.Equal(t, "tcp", lis.Addr().Network())
	require.NotNil(t, grpcServer)
}
