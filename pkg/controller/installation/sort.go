package installation

import (
	shipperV1 "github.com/bookingcom/shipper/pkg/apis/shipper/v1"
)

type byClusterName []*shipperV1.ClusterInstallationStatus

func (c byClusterName) Len() int {
	return len(c)
}

func (c byClusterName) Swap(i, j int) {
	c[i], c[j] = c[j], c[i]
}

func (c byClusterName) Less(i, j int) bool {
	return c[i].Name < c[j].Name
}