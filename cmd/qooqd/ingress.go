package main

import (
	"errors"
	"fmt"

	"github.com/qooqapp/qooq/internal/ingress"
)

// checkIngressAppDomain validates the app-domain flags together. Each check
// refuses rather than warns: these settings are the limit on what a user can
// route, and a node that started without them would serve without it.
func checkIngressAppDomain(httpsPort int, appDomain, redirect, certFile, keyFile string) error {
	if httpsPort < 1 || httpsPort > 65535 {
		return fmt.Errorf("-ingress-https-port %d is not a port", httpsPort)
	}
	if appDomain != "" {
		if err := ingress.ValidateAppDomain(appDomain); err != nil {
			return err
		}
	}
	if redirect != "" {
		if appDomain == "" {
			return errors.New("-ingress-app-domain-redirect needs -ingress-app-domain")
		}
		if err := ingress.ValidateRedirect(redirect); err != nil {
			return err
		}
	}
	if (certFile != "" || keyFile != "") && appDomain == "" {
		return errors.New("-ingress-cert-file/-ingress-key-file serve -ingress-app-domain, which is not set")
	}
	return ingress.ValidateCertPair(certFile, keyFile)
}
