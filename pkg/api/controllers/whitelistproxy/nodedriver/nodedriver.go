package nodedriver

import (
	"context"

	v3 "github.com/rancher/rancher/pkg/types/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/types/config"
	"github.com/rancher/rancher/server/whitelist"
	"k8s.io/apimachinery/pkg/runtime"
)

func Register(ctx context.Context, management *config.ScaledContext) {
	management.Management.NodeDrivers("").AddHandler(ctx, "whitelist-proxy", sync)
}

func sync(key string, nodeDriver *v3.NodeDriver) (runtime.Object, error) {
	if key == "" || nodeDriver == nil {
		return nil, nil
	}
	if nodeDriver.DeletionTimestamp != nil {
		for _, d := range nodeDriver.Spec.WhitelistDomains {
			whitelist.Proxy.Rm(d)
		}
		return nil, nil
	}

	for _, d := range nodeDriver.Spec.WhitelistDomains {
		whitelist.Proxy.Add(d)
	}
	return nil, nil
}
