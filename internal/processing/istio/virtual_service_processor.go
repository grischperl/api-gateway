package istio

import (
	"fmt"
	"time"

	gatewayv1beta1 "github.com/kyma-project/api-gateway/api/v1beta1"
	"github.com/kyma-project/api-gateway/internal/builders"
	"github.com/kyma-project/api-gateway/internal/helpers"
	"github.com/kyma-project/api-gateway/internal/processing"
	"github.com/kyma-project/api-gateway/internal/processing/processors"
	networkingv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
)

// NewVirtualServiceProcessor returns a VirtualServiceProcessor with the desired state handling specific for the Istio handler.
func NewVirtualServiceProcessor(config processing.ReconciliationConfig) processors.VirtualServiceProcessor {
	return processors.VirtualServiceProcessor{
		Creator: virtualServiceCreator{
			oathkeeperSvc:       config.OathkeeperSvc,
			oathkeeperSvcPort:   config.OathkeeperSvcPort,
			corsConfig:          config.CorsConfig,
			additionalLabels:    config.AdditionalLabels,
			defaultDomainName:   config.DefaultDomainName,
			httpTimeoutDuration: config.HTTPTimeoutDuration,
		},
	}
}

type virtualServiceCreator struct {
	oathkeeperSvc       string
	oathkeeperSvcPort   uint32
	corsConfig          *processing.CorsConfig
	defaultDomainName   string
	additionalLabels    map[string]string
	httpTimeoutDuration int
}

// Create returns the Virtual Service using the configuration of the APIRule.
func (r virtualServiceCreator) Create(api *gatewayv1beta1.APIRule) (*networkingv1beta1.VirtualService, error) {
	virtualServiceNamePrefix := fmt.Sprintf("%s-", api.ObjectMeta.Name)

	vsSpecBuilder := builders.VirtualServiceSpec()
	vsSpecBuilder.Host(helpers.GetHostWithDomain(*api.Spec.Host, r.defaultDomainName))
	vsSpecBuilder.Gateway(*api.Spec.Gateway)
	filteredRules := processing.FilterDuplicatePaths(api.Spec.Rules)

	for _, rule := range filteredRules {
		httpRouteBuilder := builders.HTTPRoute()
		serviceNamespace := helpers.FindServiceNamespace(api, &rule)
		routeDirectlyToService := false
		if !processing.IsSecured(rule) {
			routeDirectlyToService = true
		} else if processing.IsJwtSecured(rule) {
			routeDirectlyToService = true
		}

		var host string
		var port uint32

		if routeDirectlyToService {
			// Use rule level service if it exists
			if rule.Service != nil {
				host = helpers.GetHostLocalDomain(*rule.Service.Name, serviceNamespace)
				port = *rule.Service.Port
			} else {
				// Otherwise use service defined on APIRule spec level
				host = helpers.GetHostLocalDomain(*api.Spec.Service.Name, serviceNamespace)
				port = *api.Spec.Service.Port
			}
		} else {
			host = r.oathkeeperSvc
			port = r.oathkeeperSvcPort
		}

		httpRouteBuilder.Route(builders.RouteDestination().Host(host).Port(port))

		if rule.Path == "/*" {
			httpRouteBuilder.Match(builders.MatchRequest().Uri().Prefix("/"))
		} else {
			httpRouteBuilder.Match(builders.MatchRequest().Uri().Regex(rule.Path))
		}
		httpRouteBuilder.CorsPolicy(builders.CorsPolicy().
			AllowOrigins(r.corsConfig.AllowOrigins...).
			AllowMethods(r.corsConfig.AllowMethods...).
			AllowHeaders(r.corsConfig.AllowHeaders...))
		httpRouteBuilder.Timeout(time.Second * time.Duration(r.httpTimeoutDuration))

		headersBuilder := builders.NewHttpRouteHeadersBuilder().
			SetHostHeader(helpers.GetHostWithDomain(*api.Spec.Host, r.defaultDomainName))

		// We need to add mutators only for JWT secured rules, since "noop" and "oauth2_introspection" access strategies
		// create access rules and therefore use ory mutators. The "allow" access strategy does not support mutators at all.
		if processing.IsJwtSecured(rule) {
			cookieMutator, err := rule.GetCookieMutator()
			if err != nil {
				return nil, err
			}
			if cookieMutator.HasCookies() {
				headersBuilder.SetRequestCookies(cookieMutator.ToString())
			}

			headerMutator, err := rule.GetHeaderMutator()
			if err != nil {
				return nil, err
			}
			if headerMutator.HasHeaders() {
				headersBuilder.SetRequestHeaders(headerMutator.Headers)
			}
		}

		httpRouteBuilder.Headers(headersBuilder.Get())

		vsSpecBuilder.HTTP(httpRouteBuilder)

	}

	vsBuilder := builders.VirtualService().
		GenerateName(virtualServiceNamePrefix).
		Namespace(api.ObjectMeta.Namespace).
		Label(processing.OwnerLabel, fmt.Sprintf("%s.%s", api.ObjectMeta.Name, api.ObjectMeta.Namespace)).
		Label(processing.OwnerLabelv1alpha1, fmt.Sprintf("%s.%s", api.ObjectMeta.Name, api.ObjectMeta.Namespace))

	for k, v := range r.additionalLabels {
		vsBuilder.Label(k, v)
	}

	vsBuilder.Spec(vsSpecBuilder)

	return vsBuilder.Get(), nil
}
