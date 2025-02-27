// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/types"
	k8serrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentv1alpha1 "github.com/elastic/cloud-on-k8s/v2/pkg/apis/agent/v1alpha1"
	commonv1 "github.com/elastic/cloud-on-k8s/v2/pkg/apis/common/v1"
	v1 "github.com/elastic/cloud-on-k8s/v2/pkg/apis/kibana/v1"
	commonhttp "github.com/elastic/cloud-on-k8s/v2/pkg/controller/common/http"
	"github.com/elastic/cloud-on-k8s/v2/pkg/controller/common/reconciler"
	"github.com/elastic/cloud-on-k8s/v2/pkg/utils/k8s"
	"github.com/elastic/cloud-on-k8s/v2/pkg/utils/net"
	"github.com/elastic/cloud-on-k8s/v2/pkg/utils/stringsutil"
)

const FleetTokenAnnotation = "fleet.eck.k8s.elastic.co/token" //nolint:gosec

// EnrollmentAPIKeyResult wrapper for a single result in the Fleet API.
type EnrollmentAPIKeyResult struct {
	Item EnrollmentAPIKey `json:"item"`
}

// EnrollmentAPIKey is the representation of an enrollment token in the Fleet API.
type EnrollmentAPIKey struct {
	ID       string `json:"id,omitempty"`
	Active   bool   `json:"active,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	PolicyID string `json:"policy_id,omitempty"`
}

func (e EnrollmentAPIKey) isEmpty() bool {
	return !e.Active && e.ID == "" && e.APIKey == "" && e.PolicyID == ""
}

// PolicyList is a wrapper for a list of agent policies as returned by the Fleet API.
type PolicyList struct {
	Items []Policy `json:"items"`
}

// Policy is the representation of an agent policy in the Fleet API.
type Policy struct {
	ID                   string `json:"id"`
	IsDefault            bool   `json:"is_default"`
	IsDefaultFleetServer bool   `json:"is_default_fleet_server"`
	Status               string `json:"status"`
}

type fleetAPI struct {
	client        *http.Client
	endpoint      string
	username      string
	password      string
	kibanaVersion string
	log           logr.Logger
}

func newFleetAPI(dialer net.Dialer, settings connectionSettings, logger logr.Logger) fleetAPI {
	return fleetAPI{
		client:        commonhttp.Client(dialer, settings.caCerts, 60*time.Second),
		kibanaVersion: settings.version,
		endpoint:      settings.host,
		username:      settings.credentials.Username,
		password:      settings.credentials.Password,
		log:           logger,
	}
}

func (f fleetAPI) request(
	ctx context.Context,
	method string,
	pathWithQuery string,
	requestObj, responseObj interface{}) error {
	var body io.Reader = http.NoBody
	if requestObj != nil {
		outData, err := json.Marshal(requestObj)
		if err != nil {
			return err
		}
		body = bytes.NewBuffer(outData)
	}

	request, err := http.NewRequestWithContext(ctx, method, stringsutil.Concat(f.endpoint, "/api/fleet/", pathWithQuery), body)
	if err != nil {
		return err
	}

	// Sets headers allowing ES to distinguish between deprecated APIs used internally and by the user
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set(commonhttp.InternalProductRequestHeaderKey, commonhttp.InternalProductRequestHeaderValue)
	request.Header.Set("kbn-xsrf", "true")
	request.SetBasicAuth(f.username, f.password)

	f.log.V(1).Info(
		"Fleet API HTTP request",
		"method", request.Method,
		"url", request.URL.Redacted(),
	)

	resp, err := f.client.Do(request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := commonhttp.MaybeAPIError(resp); err != nil {
		return err
	}
	if responseObj != nil {
		if err := json.NewDecoder(resp.Body).Decode(responseObj); err != nil {
			return err
		}
	}

	return nil
}

func (f fleetAPI) enrollmentAPIKeyPath() string {
	path := "enrollment_api_keys"
	if strings.HasPrefix(f.kibanaVersion, "7") {
		path = "enrollment-api-keys"
	}
	return path
}

func (f fleetAPI) createEnrollmentAPIKey(ctx context.Context, policyID string) (EnrollmentAPIKey, error) {
	var response EnrollmentAPIKeyResult
	err := f.request(ctx, http.MethodPost, f.enrollmentAPIKeyPath(), EnrollmentAPIKey{PolicyID: policyID}, &response)
	return response.Item, err
}

func (f fleetAPI) getEnrollmentAPIKey(ctx context.Context, keyID string) (EnrollmentAPIKey, error) {
	var response EnrollmentAPIKeyResult
	err := f.request(ctx, http.MethodGet, fmt.Sprintf("%s/%s", f.enrollmentAPIKeyPath(), keyID), nil, &response)
	return response.Item, err
}

func (f fleetAPI) deleteEnrollmentAPIKey(ctx context.Context, keyID string) error {
	return f.request(ctx, http.MethodDelete, fmt.Sprintf("%s/%s", f.enrollmentAPIKeyPath(), keyID), nil, nil)
}

func (f fleetAPI) findAgentPolicy(ctx context.Context, filter func(policy Policy) bool) (Policy, error) {
	page := 1
	for {
		var list PolicyList
		err := f.request(ctx, http.MethodGet, fmt.Sprintf("agent_policies?perPage=20&page=%d", page), nil, &list)
		if err != nil {
			return Policy{}, err
		}
		if len(list.Items) == 0 {
			break
		}
		for _, p := range list.Items {
			if filter(p) {
				return p, nil
			}
		}
		page++
	}
	return Policy{}, errors.New("no matching agent policy found")
}

func (f fleetAPI) defaultFleetServerPolicyID(ctx context.Context) (string, error) {
	policy, err := f.findAgentPolicy(ctx, func(policy Policy) bool {
		return policy.IsDefaultFleetServer && policy.Status == "active"
	})
	if err != nil {
		return "", err
	}
	return policy.ID, nil
}

func (f fleetAPI) defaultAgentPolicyID(ctx context.Context) (string, error) {
	policy, err := f.findAgentPolicy(ctx, func(policy Policy) bool {
		return policy.IsDefault && policy.Status == "active"
	})
	if err != nil {
		return "", err
	}
	return policy.ID, nil
}

func (f fleetAPI) setupFleet(ctx context.Context) error {
	return f.request(ctx, http.MethodPost, "setup", nil, nil)
}

func maybeReconcileFleetEnrollment(params Params, result *reconciler.Results) EnrollmentAPIKey {
	if !params.Agent.Spec.KibanaRef.IsDefined() {
		return EnrollmentAPIKey{}
	}

	reachable, err := isKibanaReachable(params.Context, params.Client, params.Agent.Spec.KibanaRef.WithDefaultNamespace(params.Agent.Namespace).NamespacedName())
	if err != nil {
		result.WithError(err)
		return EnrollmentAPIKey{}
	}
	if !reachable {
		result.WithResult(reconcile.Result{Requeue: true})
		return EnrollmentAPIKey{}
	}

	kbConnectionSettings, err := extractClientConnectionSettings(params.Context, params.Agent, params.Client, commonv1.KibanaAssociationType)
	if err != nil {
		result.WithError(err)
		return EnrollmentAPIKey{}
	}

	token, err := reconcileEnrollmentToken(
		params.Context, params.Agent, params.Client,
		newFleetAPI(
			params.OperatorParams.Dialer,
			kbConnectionSettings,
			params.Logger()),
	)
	result.WithError(err)
	return token
}

func isKibanaReachable(ctx context.Context, client k8s.Client, kibanaNSN types.NamespacedName) (bool, error) {
	var kb v1.Kibana
	err := client.Get(ctx, kibanaNSN, &kb)
	if err != nil {
		return false, err
	}
	if kb.Status.Health != commonv1.GreenHealth {
		return false, nil // requeue
	}
	return true, nil
}

func reconcileEnrollmentToken(ctx context.Context, agent agentv1alpha1.Agent, client k8s.Client, api fleetAPI) (EnrollmentAPIKey, error) {
	// do we have an existing token that we have rolled out previously?
	tokenName, exists := agent.Annotations[FleetTokenAnnotation]
	if !exists {
		// setup fleet to create default policies (and tokens)
		if err := api.setupFleet(ctx); err != nil {
			return EnrollmentAPIKey{}, err
		}
	}
	// what policy should we enroll this agent in?
	policyID, err := reconcilePolicyID(ctx, agent, api)
	if err != nil {
		return EnrollmentAPIKey{}, err
	}
	if exists {
		// get the enrollment token identified by the annotation
		key, err := api.getEnrollmentAPIKey(ctx, tokenName)
		// the annotation might contain corrupted or no longer valid information
		if err != nil && commonhttp.IsNotFound(err) {
			goto CREATE
		}
		if err != nil {
			return EnrollmentAPIKey{}, err
		}
		// if the token is valid and for the right policy we are done here
		if key.Active && key.PolicyID == policyID {
			return key, nil
		}
	}

CREATE:
	key, err := api.createEnrollmentAPIKey(ctx, policyID)
	if err != nil {
		return EnrollmentAPIKey{}, err
	}
	// this potentially creates conflicts we could introduce reconciler state similar to the ES controller and handle it
	// on the top level but we would then potentially create redundant enrollment tokens in the Fleet API
	if agent.Annotations == nil {
		agent.Annotations = map[string]string{}
	}
	agent.Annotations[FleetTokenAnnotation] = key.ID
	err = client.Update(ctx, &agent)
	if err != nil {
		// we have failed to store the token id in an annotation let's try to remove the token again
		return EnrollmentAPIKey{}, k8serrors.NewAggregate([]error{err, api.deleteEnrollmentAPIKey(ctx, key.ID)})
	}
	return key, nil
}

func reconcilePolicyID(ctx context.Context, agent agentv1alpha1.Agent, api fleetAPI) (string, error) {
	if agent.Spec.PolicyID != "" {
		return agent.Spec.PolicyID, nil
	}
	if agent.Spec.FleetServerEnabled {
		return api.defaultFleetServerPolicyID(ctx)
	}
	return api.defaultAgentPolicyID(ctx)
}
