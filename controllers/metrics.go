package controllers

import (
	dbaasv1alpha1 "github.com/RHEcosystemAppEng/dbaas-operator/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	PlatformStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "dbaas_platform_status",
			Help: "status of an installation of components and provider operators",
		},
		[]string{
			"platform",
			"status",
		},
	)
)

// SetPlatformStatus exposes dbaas_platform_status metric for each platform
func SetPlatformStatusMetric(platformName dbaasv1alpha1.PlatformsName, status dbaasv1alpha1.PlatformsInstlnStatus) {
	//PlatformStatus.Reset()
	if len(platformName) > 0 {
		PlatformStatus.With(prometheus.Labels{"platform": string(platformName), "status": string(status)}).Set(float64(1))
	}
}
