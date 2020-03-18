// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package whitelist

import (
	"fmt"

	"github.com/elastic/cloud-on-k8s/hack/licence-detector/dependency"
)

// CheckWhitelist returns an error if any dependencies use a licence that is not permitted by Elastic
func CheckWhitelist(deps *dependency.List) error {
	// this is not an exhaustive list of Elastic-approved licences, but includes all the ones we use to date
	whitelist := []string{
		"Apache-2.0",
		"BSD-2-Clause",
		"BSD-3-Clause",
		"ISC",
		"MIT",
		// Yellow list: Mozilla Public License 1.1 or 2.0 (“MPL”) Exception:
		// "Incorporation of unmodified source or binaries into Elastic products is permitted,
		// provided that the product's NOTICE file links to a URL providing the MPL-covered source code"
		// We do not modify any of the dependencies and we link to the source code, so we are okay.
		"MPL-2.0",
		"Public Domain",
		// TODO: remove this once github.com/dgryski/go-gk is removed or licenced
		"UNKNOWN",
	}

	err := "Dependency %s uses licence %s which is not whitelisted"
	for _, dep := range deps.Direct {
		if !(inSlice(dep.LicenceType, whitelist)) {
			return fmt.Errorf(err, dep.Name, dep.LicenceType)
		}
	}
	for _, dep := range deps.Indirect {
		if !(inSlice(dep.LicenceType, whitelist)) {
			return fmt.Errorf(err, dep.Name, dep.LicenceType)
		}
	}
	return nil
}

func inSlice(item string, slice []string) bool {
	for _, val := range slice {
		if item == val {
			return true
		}
	}
	return false
}
