package config

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"

	"hhc/asset-api/internal/assets"
)

const BulletinExtractorCaller = "hhc-web-extractor"

type WorkloadAuth struct {
	Issuer                 string                                 `json:"issuer"`
	Audience               string                                 `json:"audience"`
	JWKSURL                string                                 `json:"jwksUrl"`
	PrincipalClaim         string                                 `json:"principalClaim"`
	Principals             map[string]string                      `json:"principals"`
	DisableLegacyCallers   []string                               `json:"disableLegacyCallers"`
	OwnerContentPrincipals []string                               `json:"ownerContentPrincipals"`
	UploadProfileScopes    map[string][]assets.UploadProfileScope `json:"uploadProfileScopes"`
}

func loadWorkloadAuth(raw string, cfg Config) (*WorkloadAuth, error) {
	if raw == "" {
		return nil, nil
	}
	invalid := errors.New("invalid ASSET_WORKLOAD_AUTH configuration")
	if len(raw) > 32*1024 {
		return nil, invalid
	}
	var auth WorkloadAuth
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&auth); err != nil {
		return nil, invalid
	}
	if decoder.Decode(&struct{}{}) != io.EOF || !workloadHTTPSURL(auth.Issuer) || !workloadHTTPSURL(auth.JWKSURL) ||
		!workloadValue(auth.Audience) || (auth.PrincipalClaim != "oid" && auth.PrincipalClaim != "sub") ||
		len(auth.Principals) == 0 || len(auth.Principals) > 64 || len(auth.DisableLegacyCallers) > 64 {
		return nil, invalid
	}
	allowed := func(caller string) bool {
		return caller != "" && (cfg.AllowedCallers[caller] || caller == cfg.ReaderCallerAppID ||
			((caller == "hhc-web-bulletin-worker" || caller == BulletinExtractorCaller) && cfg.AllowedCallers["hhc-web-api"]))
	}
	for principal, caller := range auth.Principals {
		if !workloadValue(principal) || !allowed(caller) {
			return nil, invalid
		}
	}
	for _, caller := range auth.DisableLegacyCallers {
		if !allowed(caller) {
			return nil, invalid
		}
	}
	for principal, scopes := range auth.UploadProfileScopes {
		caller := auth.Principals[principal]
		if caller == "" || len(scopes) == 0 || len(scopes) > 16 {
			return nil, invalid
		}
		for _, scope := range scopes {
			if !scope.ValidForCaller(caller) {
				return nil, invalid
			}
		}
	}
	if len(auth.OwnerContentPrincipals) > 64 {
		return nil, invalid
	}
	for _, principal := range auth.OwnerContentPrincipals {
		if auth.Principals[principal] != "hhc-web-api" {
			return nil, invalid
		}
	}
	return &auth, nil
}

func workloadHTTPSURL(value string) bool {
	target, err := url.Parse(value)
	return err == nil && target.Scheme == "https" && target.Hostname() != "" && target.User == nil &&
		target.Fragment == "" && target.RawQuery == "" && !target.ForceQuery && workloadValue(value)
}

func workloadValue(value string) bool {
	return value != "" && len(value) <= 255 && !strings.ContainsAny(value, " \r\n\t")
}
