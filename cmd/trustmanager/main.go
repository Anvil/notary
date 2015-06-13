package main

import (
	"crypto/x509"
	"fmt"
	"os"
	"os/user"
	"path"
	"time"

	"github.com/codegangsta/cli"
	"github.com/spf13/viper"

	"github.com/docker/vetinari/trustmanager"
)

const configFileName string = "config"
const configPath string = ".docker/trust/"
const caDir string = ".docker/trust/certificate_authorities/"

var caStore trustmanager.X509Store

func init() {
	// Retrieve current user to get home directory
	usr, err := user.Current()
	if err != nil {
		fatalf("cannot get current user: %v", err)
	}

	// Get home directory for current user
	homeDir := usr.HomeDir
	if homeDir == "" {
		fatalf("cannot get current user home directory")
	}

	// Setup the configuration details
	viper.SetConfigName(configFileName)
	viper.AddConfigPath(path.Join(homeDir, path.Dir(configPath)))
	viper.SetConfigType("json")

	// Find and read the config file
	err = viper.ReadInConfig()
	if err != nil {
		// Ignore if the configuration file doesn't exist, we can use the defaults
		if !os.IsNotExist(err) {
			panic(fmt.Errorf("Fatal error config file: %s \n", err))
		}
	}

	// Set up the defaults for our config
	viper.SetDefault("caDir", path.Join(homeDir, path.Dir(caDir)))

	// Get the final value for the CA directory
	finalcaDir := viper.GetString("caDir")

	// Ensure the existence of the CAs directory
	createDirectory(finalcaDir)

	// Load all CAs that have BasicConstrains valid, are an actual CA, aren't expired and don't use SHA1
	caStore = trustmanager.NewX509FilteredFileStore(finalcaDir, func(cert *x509.Certificate) bool {
		return cert.IsCA && cert.BasicConstraintsValid && time.Now().Before(cert.NotAfter) && cert.SignatureAlgorithm != x509.SHA1WithRSA && cert.SignatureAlgorithm != x509.DSAWithSHA1 && cert.SignatureAlgorithm != x509.ECDSAWithSHA1
	})
}

func main() {
	app := cli.NewApp()
	app.Name = "keymanager"
	app.Usage = "trust keymanager"

	app.Commands = []cli.Command{
		commandAdd,
		commandList,
		commandRemove,
	}

	app.RunAndExitOnError()
}

func fatalf(format string, args ...interface{}) {
	fmt.Println("* fatal: ", format)
	fmt.Println(args...)
	os.Exit(1)
}

func createDirectory(dir string) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		fatalf("cannot create directory: %v", err)
	}
}