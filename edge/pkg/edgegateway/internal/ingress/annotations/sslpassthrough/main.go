/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sslpassthrough

import (
	"github.com/kubeedge/kubeedge/edge/pkg/edgegateway/internal/ingress/annotations/parser"
	"github.com/kubeedge/kubeedge/edge/pkg/edgegateway/internal/ingress/errors"
	"github.com/kubeedge/kubeedge/edge/pkg/edgegateway/internal/ingress/resolver"
	networking "k8s.io/api/networking/v1beta1"
)

type sslpt struct {
	r resolver.Resolver
}

// NewParser creates a new SSL passthrough annotation parser
func NewParser(r resolver.Resolver) parser.IngressAnnotation {
	return sslpt{r}
}

// ParseAnnotations parses the annotations contained in the ingress
// rule used to indicate if is required to configure
func (a sslpt) Parse(ing *networking.Ingress) (interface{}, error) {
	if ing.GetAnnotations() == nil {
		return false, errors.ErrMissingAnnotations
	}

	return parser.GetBoolAnnotation("ssl-passthrough", ing)
}
