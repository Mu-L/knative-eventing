/*
Copyright 2019 The Knative Authors

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

package main

import (
	"context"
	"log"
	"time"

	// Uncomment the following line to load the gcp plugin (only required to authenticate against GKE clusters).
	// _ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	"github.com/kelseyhightower/envconfig"
	"go.uber.org/zap"
	configmapinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/configmap/filtered"

	kubeclient "knative.dev/pkg/client/injection/kube/client"
	filteredFactory "knative.dev/pkg/client/injection/kube/informers/factory/filtered"
	configmap "knative.dev/pkg/configmap/informer"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/injection"
	"knative.dev/pkg/logging"
	k8sruntime "knative.dev/pkg/observability/runtime/k8s"
	"knative.dev/pkg/signals"
	"knative.dev/pkg/system"

	cmdbroker "knative.dev/eventing/cmd/broker"
	"knative.dev/eventing/pkg/apis/feature"
	"knative.dev/eventing/pkg/auth"
	"knative.dev/eventing/pkg/broker"
	"knative.dev/eventing/pkg/broker/ingress"
	eventingclient "knative.dev/eventing/pkg/client/injection/client"
	brokerinformer "knative.dev/eventing/pkg/client/injection/informers/eventing/v1/broker"
	eventpolicyinformer "knative.dev/eventing/pkg/client/injection/informers/eventing/v1alpha1/eventpolicy"
	eventtypeinformer "knative.dev/eventing/pkg/client/injection/informers/eventing/v1beta3/eventtype"
	"knative.dev/eventing/pkg/eventingtls"
	"knative.dev/eventing/pkg/eventtype"
	o11yconfigmap "knative.dev/eventing/pkg/observability/configmap"
	"knative.dev/eventing/pkg/observability/otel"
)

// TODO make these constants configurable (either as env variables, config map, or part of broker spec).
//
//	Issue: https://github.com/knative/eventing/issues/1777
const (
	// Constants for the underlying HTTP Client transport. These would enable better connection reuse.
	// Purposely set them to be equal, as the ingress only connects to its channel.
	// These are magic numbers, partly set based on empirical evidence running performance workloads, and partly
	// based on what serving is doing. See https://github.com/knative/serving/blob/main/pkg/network/transports.go.
	defaultMaxIdleConnections        = 1000
	defaultMaxIdleConnectionsPerHost = 1000
	defaultMetricsPort               = 9092
	component                        = "mt_broker_ingress"
)

type envConfig struct {
	// TODO: change this environment variable to something like "PodGroupName".
	PodName       string `envconfig:"POD_NAME" required:"true"`
	ContainerName string `envconfig:"CONTAINER_NAME" required:"true"`
	Port          int    `envconfig:"INGRESS_PORT" default:"8080"`
	MaxTTL        int32  `envconfig:"MAX_TTL" default:"255"`
	HTTPPort      int    `envconfig:"INGRESS_PORT" default:"8080"`
	HTTPSPort     int    `envconfig:"INGRESS_PORT_HTTPS" default:"8443"`
}

func main() {
	ctx := signals.NewContext()

	cfg := injection.ParseAndGetRESTConfigOrDie()
	ctx = injection.WithConfig(ctx, cfg)

	var env envConfig
	if err := envconfig.Process("", &env); err != nil {
		log.Fatal("Failed to process env var", zap.Error(err))
	}

	if env.MaxTTL <= 0 {
		log.Fatalf("Invalid MaxTTL value, must be >=0, was: %d", env.MaxTTL)
	}

	log.Printf("Using TTL of %d", env.MaxTTL)
	log.Printf("Registering %d clients", len(injection.Default.GetClients()))
	log.Printf("Registering %d informer factories", len(injection.Default.GetInformerFactories()))
	log.Printf("Registering %d informers", len(injection.Default.GetInformers()))

	ctx = filteredFactory.WithSelectors(ctx,
		auth.OIDCLabelSelector,
		eventingtls.TrustBundleLabelSelector,
	)

	ctx, informers := injection.Default.SetupInformers(ctx, cfg)
	ctx = injection.WithConfig(ctx, cfg)
	loggingConfig, err := cmdbroker.GetLoggingConfig(ctx, system.Namespace(), logging.ConfigMapName())
	if err != nil {
		log.Fatal("Error loading/parsing logging configuration:", err)
	}
	sl, atomicLevel := logging.NewLoggerFromConfig(loggingConfig, component)
	logger := sl.Desugar()
	defer flush(sl)

	pprof := k8sruntime.NewProfilingServer(sl.Named("pprof"))

	mp, tp := otel.SetupObservabilityOrDie(ctx, "broker.ingress", sl, pprof)

	defer func() {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		if err := mp.Shutdown(ctx); err != nil {
			sl.Errorw("Error flushing metrics", zap.Error(err))
		}

		if err := tp.Shutdown(ctx); err != nil {
			sl.Errorw("Error flushing traces", zap.Error(err))
		}
	}()

	logger.Info("Starting the Broker Ingress")

	brokerInformer := brokerinformer.Get(ctx)

	// Watch the logging config map and dynamically update logging levels.
	configMapWatcher := configmap.NewInformedWatcher(kubeclient.Get(ctx), system.Namespace())

	configMapWatcher.Watch(o11yconfigmap.Name(), pprof.UpdateFromConfigMap)
	// TODO change the component name to broker once Stackdriver metrics are approved.
	// Watch the observability config map and dynamically update request logs.
	configMapWatcher.Watch(logging.ConfigMapName(), logging.UpdateLevelFromConfigMap(sl, atomicLevel, component))

	trustBundleConfigMapLister := configmapinformer.Get(ctx, eventingtls.TrustBundleLabelSelector).Lister().ConfigMaps(system.Namespace())

	var featureStore *feature.Store
	var handler *ingress.Handler

	featureStore = feature.NewStore(logging.FromContext(ctx).Named("feature-config-store"), func(name string, value interface{}) {
		featureFlags := value.(feature.Flags)
		if featureFlags.IsEnabled(feature.EvenTypeAutoCreate) && featureStore != nil && handler != nil {
			autoCreate := &eventtype.EventTypeAutoHandler{
				EventTypeLister: eventtypeinformer.Get(ctx).Lister(),
				EventingClient:  eventingclient.Get(ctx).EventingV1beta3(),
				FeatureStore:    featureStore,
				Logger:          logger,
			}
			handler.EvenTypeHandler = autoCreate
		}
	})
	featureStore.WatchConfigs(configMapWatcher)

	// Decorate contexts with the current state of the feature config.
	ctxFunc := func(ctx context.Context) context.Context {
		return featureStore.ToContext(ctx)
	}

	oidcTokenProvider := auth.NewOIDCTokenProvider(ctx)
	authVerifier := auth.NewVerifier(ctx, eventpolicyinformer.Get(ctx).Lister(), trustBundleConfigMapLister, configMapWatcher)
	handler, err = ingress.NewHandler(
		logger,
		broker.TTLDefaulter(logger, env.MaxTTL),
		brokerInformer,
		authVerifier,
		oidcTokenProvider,
		trustBundleConfigMapLister,
		ctxFunc,
		mp,
		tp,
	)
	if err != nil {
		logger.Fatal("Error creating Handler", zap.Error(err))
	}

	serverManager, err := ingress.NewServerManager(
		ctx,
		logger,
		configMapWatcher,
		env.HTTPPort,
		env.HTTPSPort,
		mp,
		tp,
		handler,
	)
	if err != nil {
		logger.Fatal("Error creating server manager", zap.Error(err))
	}

	// configMapWatcher does not block, so start it first.
	if err = configMapWatcher.Start(ctx.Done()); err != nil {
		logger.Warn("Failed to start ConfigMap watcher", zap.Error(err))
	}

	// Init auto-create only if enabled, after ConfigMap watcher is started
	if featureStore.IsEnabled(feature.EvenTypeAutoCreate) {
		autoCreate := &eventtype.EventTypeAutoHandler{
			EventTypeLister: eventtypeinformer.Get(ctx).Lister(),
			EventingClient:  eventingclient.Get(ctx).EventingV1beta3(),
			FeatureStore:    featureStore,
			Logger:          logger,
		}
		handler.EvenTypeHandler = autoCreate
	}

	// Start all of the informers and wait for them to sync.
	logger.Info("Starting informers.")
	if err := controller.StartInformers(ctx.Done(), informers...); err != nil {
		logger.Fatal("Failed to start informers", zap.Error(err))
	}

	// Start the servers
	logger.Info("Ingress starting...")
	err = serverManager.StartServers(ctx)
	if err != nil {
		logger.Fatal("serverManager.StartServers() returned an error", zap.Error(err))
	}
}

func flush(logger *zap.SugaredLogger) {
	_ = logger.Sync()
}
