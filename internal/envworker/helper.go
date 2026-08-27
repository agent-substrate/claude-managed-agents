// Copyright 2023 Anthropic, PBC. Licensed under the MIT License; see the
// LICENSE file in this directory.

package envworker

import "github.com/anthropics/anthropic-sdk-go/option"

// helperTag is the x-stainless-helper telemetry value the SDK stamps on the
// requests its helpers issue. The upstream package takes these from an
// internal SDK package that cannot be imported from outside the module; the
// values are kept verbatim so the traffic is attributed the same way.
type helperTag string

const (
	helperEnvironmentsWorkPoller helperTag = "environments-work-poller"
	helperEnvironmentsWorker     helperTag = "environments-worker"
)

// withHelperTag stamps the telemetry header on one request.
func withHelperTag(v helperTag) option.RequestOption {
	return option.WithHeader("x-stainless-helper", string(v))
}
