package certs

import (
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/notary"
	"github.com/docker/notary/trustmanager"
	"github.com/docker/notary/tuf/data"
	"github.com/docker/notary/tuf/signed"
	"github.com/docker/notary/tuf/utils"
	"strings"
)

// ErrValidationFail is returned when there is no valid trusted certificates
// being served inside of the roots.json
type ErrValidationFail struct {
	Reason string
}

// ErrValidationFail is returned when there is no valid trusted certificates
// being served inside of the roots.json
func (err ErrValidationFail) Error() string {
	return fmt.Sprintf("could not validate the path to a trusted root: %s", err.Reason)
}

// ErrRootRotationFail is returned when we fail to do a full root key rotation
// by either failing to add the new root certificate, or delete the old ones
type ErrRootRotationFail struct {
	Reason string
}

// ErrRootRotationFail is returned when we fail to do a full root key rotation
// by either failing to add the new root certificate, or delete the old ones
func (err ErrRootRotationFail) Error() string {
	return fmt.Sprintf("could not rotate trust to a new trusted root: %s", err.Reason)
}

/*
ValidateRoot receives a new root, validates its correctness and attempts to
do root key rotation if needed.

First we list the current trusted certificates we have for a particular GUN. If
that list is non-empty means that we've already seen this repository before, and
have a list of trusted certificates for it. In this case, we use this list of
certificates to attempt to validate this root file.

If the previous validation succeeds, we check the integrity of the root by
making sure that it is validated by itself. This means that we will attempt to
validate the root data with the certificates that are included in the root keys
themselves.

However, if we do not have any current trusted certificates for this GUN, we
check if there are any pinned certificates specified in the trust_pinning section
of the notary client config.  If this section specifies a Certs section with this
GUN, we attempt to validate that the certificates present in the downloaded root
file match the pinned ID.

If the Certs section is empty for this GUN, we check if the trust_pinning
section specifies a CA section specified in the config for this GUN.  If so, we check
that the specified CA is valid and has signed a certificate included in the downloaded
root file.  The specified CA can be a prefix for this GUN.

If both the Certs and CA configs do not match this GUN, we fall back to the TOFU
section in the config: if true, we trust certificates specified in the root for
this GUN. If later we see a different certificate for that certificate, we return
an ErrValidationFailed error.

Note that since we only allow trust data to be downloaded over an HTTPS channel
we are using the current public PKI to validate the first download of the certificate
adding an extra layer of security over the normal (SSH style) trust model.
We shall call this: TOFUS.

Validation failure at any step will result in an ErrValidationFailed error.
*/
func ValidateRoot(certStore trustmanager.X509Store, root *data.Signed, gun string, trustPinning notary.TrustPinConfig) error {
	logrus.Debugf("entered ValidateRoot with dns: %s", gun)
	signedRoot, err := data.RootFromSigned(root)
	if err != nil {
		return err
	}

	// Retrieve all the leaf certificates in root for which the CN matches the GUN
	allValidCerts, err := validRootLeafCerts(signedRoot, gun)
	if err != nil {
		logrus.Debugf("error retrieving valid leaf certificates for: %s, %v", gun, err)
		return &ErrValidationFail{Reason: "unable to retrieve valid leaf certificates"}
	}

	// Retrieve all the trusted certificates that match this gun
	certsForCN, err := certStore.GetCertificatesByCN(gun)
	if err != nil {
		// If the error that we get back is different than ErrNoCertificatesFound
		// we couldn't check if there are any certificates with this CN already
		// trusted. Let's take the conservative approach and return a failed validation
		if _, ok := err.(*trustmanager.ErrNoCertificatesFound); !ok {
			logrus.Debugf("error retrieving trusted certificates for: %s, %v", gun, err)
			return &ErrValidationFail{Reason: "unable to retrieve trusted certificates"}
		}
	}
	// If we have certificates that match this specific GUN, let's make sure to
	// use them first to validate that this new root is valid.
	if len(certsForCN) != 0 {
		logrus.Debugf("found %d valid root certificates for %s", len(certsForCN), gun)
		err = signed.VerifyRoot(root, 0, trustmanager.CertsToKeys(certsForCN))
		if err != nil {
			logrus.Debugf("failed to verify TUF data for: %s, %v", gun, err)
			return &ErrValidationFail{Reason: "failed to validate data with current trusted certificates"}
		}
	} else {
		logrus.Debugf("found no currently valid root certificates for %s", gun)
		logrus.Debugf("using trust_pinning config to bootstrap trust: %v", trustPinning)
		// First, check if the Certs section is specified for our GUN.
		// If so, we try to find a matching Cert that is pinned to bootstrap trust from
		if pinnedID, ok := trustPinning.Certs[gun]; ok {
			foundCertIDMatch := false
			for _, cert := range allValidCerts {
				// Try to match by CertID or public key ID
				certID, err := trustmanager.FingerprintCert(cert)
				if err != nil {
					continue
				}
				if certID == pinnedID {
					// If we found our pinned cert, only use that one certificate as allValidCerts for verification
					allValidCerts = []*x509.Certificate{cert}
					foundCertIDMatch = true
					break
				}
			}
			// If we didn't find any entries under our GUN in Certs with a matching certificate ID, fail validation
			if !foundCertIDMatch {
				return &ErrValidationFail{Reason: "failed to find matching certificate ID "}
			}
		} else if utils.ContainsKeyPrefix(trustPinning.CA, gun) {
			// Next, check if the CA section is specified with a GUN that prefixes our GUN.  If so, we use this CA to bootstrap trust:
			// We attempt to use the CA PEM if it's valid to add all certs to our certStore that are signed from this cert
			if len(trustPinning.CA) > 0 {
				for caGunPrefix, caFilepath := range trustPinning.CA {
					if strings.HasPrefix(gun, caGunPrefix) {
						// Try to add the CA cert to our certificate store,
						// and use it to validate certs in the root.json later
						caCert, err := trustmanager.LoadCertFromFile(caFilepath)
						if err != nil {
							return &ErrValidationFail{Reason: "failed to load specified CA trust pin"}
						}
						if err = trustmanager.ValidateCertificate(caCert); err != nil {
							return &ErrValidationFail{Reason: "failed to validate specified CA trust pin"}
						}
						// Now only consider certificates that are direct children from this CA cert, overwriting allValidCerts
						caRootPool := x509.NewCertPool()
						caRootPool.AddCert(caCert)
						validCertsForCA := []*x509.Certificate{}
						if err != nil {
							logrus.Debugf("error retrieving valid leaf certificates for: %s, %v", gun, err)
							return &ErrValidationFail{Reason: "unable to retrieve valid leaf certificates"}
						}
						for _, cert := range allValidCerts {
							certID, err := trustmanager.FingerprintCert(cert)
							if err != nil {
								logrus.Debugf("error while fingerprinting certificate with keyID: %v", err)
								continue
							}
							// Use intermediate certificates included in the root TUF metadata for our validation
							caIntPool := x509.NewCertPool()
							_, intermediateCerts := parseAllCerts(signedRoot)
							if intermediateCertList, ok := intermediateCerts[certID]; ok {
								for _, intCert := range intermediateCertList {
									caIntPool.AddCert(intCert)
								}
							}
							// Attempt to find a valid certificate chain from the leaf cert to CA root
							// Use this certificate if such a valid chain exists (possibly using intermediates)
							if _, err = cert.Verify(x509.VerifyOptions{Roots: caRootPool, Intermediates: caIntPool}); err == nil {
								validCertsForCA = append(validCertsForCA, cert)
							}
						}
						allValidCerts = validCertsForCA
					}
				}
			}
		} else if !trustPinning.TOFU {
			// If we reach this if-case, it means that we didn't find any local certs/CAs for this GUN,
			// nor did we specify any specifications for how to bootstrap trust in trust_pinning using TOFU
			// If TOFU is true, we fall through and consider all certificates
			return &ErrValidationFail{Reason: "could not bootstrap trust without trust_pinning configuration"}
		}
	}

	// Validate the integrity of the new root (does it have valid signatures)
	// Note that allValidCerts is guaranteed to be unchanged only if we had prior cert data for this GUN or enabled TOFUS
	// If we attempted to pin a certain certificate or CA, allValidCerts could have been pruned accordingly
	err = signed.VerifyRoot(root, 0, trustmanager.CertsToKeys(allValidCerts))
	if err != nil {
		logrus.Debugf("failed to verify TUF data for: %s, %v", gun, err)
		return &ErrValidationFail{Reason: "failed to validate integrity of roots"}
	}

	// Getting here means A) we had trusted certificates and both the
	// old and new validated this root; or B) we had no trusted certificates but
	// the new set of certificates passed our specified trust_pinning or default TOFUS
	logrus.Debugf("entering root certificate rotation for: %s", gun)

	// Do root certificate rotation: we trust only the certs present in the new root
	// First we add all the new certificates (even if they already exist)
	for _, cert := range allValidCerts {
		err := certStore.AddCert(cert)
		if err != nil {
			// If the error is already exists we don't fail the rotation
			if _, ok := err.(*trustmanager.ErrCertExists); ok {
				logrus.Debugf("ignoring certificate addition to: %s", gun)
				continue
			}
			logrus.Debugf("error adding new trusted certificate for: %s, %v", gun, err)
		}
	}

	// Now we delete old certificates that aren't present in the new root
	for certID, cert := range certsToRemove(certsForCN, allValidCerts) {
		logrus.Debugf("removing certificate with certID: %s", certID)
		err = certStore.RemoveCert(cert)
		if err != nil {
			logrus.Debugf("failed to remove trusted certificate with keyID: %s, %v", certID, err)
			return &ErrRootRotationFail{Reason: "failed to rotate root keys"}
		}
	}

	logrus.Debugf("Root validation succeeded for %s", gun)
	return nil
}

// validRootLeafCerts returns a list of non-expired, non-sha1 certificates whose
// Common-Names match the provided GUN
func validRootLeafCerts(root *data.SignedRoot, gun string) ([]*x509.Certificate, error) {
	// Get a list of all of the leaf certificates present in root
	allLeafCerts, _ := parseAllCerts(root)
	var validLeafCerts []*x509.Certificate

	// Go through every leaf certificate and check that the CN matches the gun
	for _, cert := range allLeafCerts {
		// Validate that this leaf certificate has a CN that matches the exact gun
		if cert.Subject.CommonName != gun {
			logrus.Debugf("error leaf certificate CN: %s doesn't match the given GUN: %s",
				cert.Subject.CommonName, gun)
			continue
		}
		// Make sure the certificate is not expired
		if time.Now().After(cert.NotAfter) {
			logrus.Debugf("error leaf certificate is expired")
			continue
		}

		// We don't allow root certificates that use SHA1
		if cert.SignatureAlgorithm == x509.SHA1WithRSA ||
			cert.SignatureAlgorithm == x509.DSAWithSHA1 ||
			cert.SignatureAlgorithm == x509.ECDSAWithSHA1 {

			logrus.Debugf("error certificate uses deprecated hashing algorithm (SHA1)")
			continue
		}

		validLeafCerts = append(validLeafCerts, cert)
	}

	if len(validLeafCerts) < 1 {
		logrus.Debugf("didn't find any valid leaf certificates for %s", gun)
		return nil, errors.New("no valid leaf certificates found in any of the root keys")
	}

	logrus.Debugf("found %d valid leaf certificates for %s", len(validLeafCerts), gun)
	return validLeafCerts, nil
}

// parseAllCerts returns two maps, one with all of the leafCertificates and one
// with all the intermediate certificates found in signedRoot
func parseAllCerts(signedRoot *data.SignedRoot) (map[string]*x509.Certificate, map[string][]*x509.Certificate) {
	leafCerts := make(map[string]*x509.Certificate)
	intCerts := make(map[string][]*x509.Certificate)

	// Before we loop through all root keys available, make sure any exist
	rootRoles, ok := signedRoot.Signed.Roles["root"]
	if !ok {
		logrus.Debugf("tried to parse certificates from invalid root signed data")
		return nil, nil
	}

	logrus.Debugf("found the following root keys: %v", rootRoles.KeyIDs)
	// Iterate over every keyID for the root role inside of roots.json
	for _, keyID := range rootRoles.KeyIDs {
		// check that the key exists in the signed root keys map
		key, ok := signedRoot.Signed.Keys[keyID]
		if !ok {
			logrus.Debugf("error while getting data for keyID: %s", keyID)
			continue
		}

		// Decode all the x509 certificates that were bundled with this
		// Specific root key
		decodedCerts, err := trustmanager.LoadCertBundleFromPEM(key.Public())
		if err != nil {
			logrus.Debugf("error while parsing root certificate with keyID: %s, %v", keyID, err)
			continue
		}

		// Get all non-CA certificates in the decoded certificates
		leafCertList := trustmanager.GetLeafCerts(decodedCerts)

		// If we got no leaf certificates or we got more than one, fail
		if len(leafCertList) != 1 {
			logrus.Debugf("invalid chain due to leaf certificate missing or too many leaf certificates for keyID: %s", keyID)
			continue
		}

		// Get the ID of the leaf certificate
		leafCert := leafCertList[0]
		leafID, err := trustmanager.FingerprintCert(leafCert)
		if err != nil {
			logrus.Debugf("error while fingerprinting root certificate with keyID: %s, %v", keyID, err)
			continue
		}

		// Store the leaf cert in the map
		leafCerts[leafID] = leafCert

		// Get all the remainder certificates marked as a CA to be used as intermediates
		intermediateCerts := trustmanager.GetIntermediateCerts(decodedCerts)
		intCerts[leafID] = intermediateCerts
	}

	return leafCerts, intCerts
}

// certsToRemove returns all the certificates from oldCerts that aren't present
// in newCerts
func certsToRemove(oldCerts, newCerts []*x509.Certificate) map[string]*x509.Certificate {
	certsToRemove := make(map[string]*x509.Certificate)

	// If no newCerts were provided
	if len(newCerts) == 0 {
		return certsToRemove
	}

	// Populate a map with all the IDs from newCert
	var newCertMap = make(map[string]struct{})
	for _, cert := range newCerts {
		certID, err := trustmanager.FingerprintCert(cert)
		if err != nil {
			logrus.Debugf("error while fingerprinting root certificate with keyID: %s, %v", certID, err)
			continue
		}
		newCertMap[certID] = struct{}{}
	}

	// Iterate over all the old certificates and check to see if we should remove them
	for _, cert := range oldCerts {
		certID, err := trustmanager.FingerprintCert(cert)
		if err != nil {
			logrus.Debugf("error while fingerprinting root certificate with certID: %s, %v", certID, err)
			continue
		}
		if _, ok := newCertMap[certID]; !ok {
			certsToRemove[certID] = cert
		}
	}

	return certsToRemove
}
