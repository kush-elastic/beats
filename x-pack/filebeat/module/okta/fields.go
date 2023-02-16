// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

// Code generated by beats/dev-tools/cmd/asset/asset.go - DO NOT EDIT.

package okta

import (
	"github.com/elastic/beats/v7/libbeat/asset"
)

func init() {
	if err := asset.SetFields("filebeat", "okta", asset.ModuleFieldsPri, AssetOkta); err != nil {
		panic(err)
	}
}

// AssetOkta returns asset data.
// This is the base64 encoded zlib format compressed contents of module/okta.
func AssetOkta() string {
	return "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"
}
