package server

// DO NOT EDIT -- generated file

import (
	"context"
	"crypto/x509"
	"fmt"
	"github.com/fluxcd/notification-controller/api/v1beta2"
	"github.com/fluxcd/notification-controller/internal/notifier"
	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/events"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"regexp"
	"sigs.k8s.io/yaml"
)

// getNotifierForCommitStatuses -- returns a notifier object for the listed provider
func (s *EventServer) getNotifierForCommitStatuses(alert v1beta2.CommitStatus, err error, ctx context.Context) (string, notifier.Interface, bool) {
	var provider v1beta2.Provider
	providerName := types.NamespacedName{Namespace: alert.Namespace, Name: alert.Spec.ProviderRef.Name}

	err = s.kubeClient.Get(ctx, providerName, &provider)
	if err != nil {
		s.logger.Error(err, "failed to read provider",
			"reconciler kind", v1beta2.ProviderKind,
			"name", providerName.Name,
			"namespace", providerName.Namespace)
		return "", nil, true // cont calling loop
	}

	if provider.Spec.Suspend {
		return "", nil, true // cont calling loop
	}

	webhook := provider.Spec.Address
	username := provider.Spec.Username
	proxy := provider.Spec.Proxy
	token := ""
	password := ""
	headers := make(map[string]string)
	if provider.Spec.SecretRef != nil {
		var secret corev1.Secret
		secretName := types.NamespacedName{Namespace: alert.Namespace, Name: provider.Spec.SecretRef.Name}

		err = s.kubeClient.Get(ctx, secretName, &secret)
		if err != nil {
			s.logger.Error(err, "failed to read secret",
				"reconciler kind", v1beta2.ProviderKind,
				"name", providerName.Name,
				"namespace", providerName.Namespace)
			return "", nil, true // cont calling loop
		}

		if address, ok := secret.Data["address"]; ok {
			webhook = string(address)
		}

		if p, ok := secret.Data["password"]; ok {
			password = string(p)
		}

		if p, ok := secret.Data["proxy"]; ok {
			proxy = string(p)
		}

		if t, ok := secret.Data["token"]; ok {
			token = string(t)
		}

		if u, ok := secret.Data["username"]; ok {
			username = string(u)
		}

		if h, ok := secret.Data["headers"]; ok {
			err := yaml.Unmarshal(h, &headers)
			if err != nil {
				s.logger.Error(err, "failed to read headers from secret",
					"reconciler kind", v1beta2.ProviderKind,
					"name", providerName.Name,
					"namespace", providerName.Namespace)
				return "", nil, true // cont calling loop
			}
		}
	}

	var certPool *x509.CertPool
	if provider.Spec.CertSecretRef != nil {
		var secret corev1.Secret
		secretName := types.NamespacedName{Namespace: alert.Namespace, Name: provider.Spec.CertSecretRef.Name}

		err = s.kubeClient.Get(ctx, secretName, &secret)
		if err != nil {
			s.logger.Error(err, "failed to read secret",
				"reconciler kind", v1beta2.ProviderKind,
				"name", providerName.Name,
				"namespace", providerName.Namespace)
			return "", nil, true // cont calling loop
		}

		caFile, ok := secret.Data["caFile"]
		if !ok {
			s.logger.Error(err, "failed to read secret key caFile",
				"reconciler kind", v1beta2.ProviderKind,
				"name", providerName.Name,
				"namespace", providerName.Namespace)
			return "", nil, true // cont calling loop
		}

		certPool = x509.NewCertPool()
		ok = certPool.AppendCertsFromPEM(caFile)
		if !ok {
			s.logger.Error(err, "could not append to cert pool",
				"reconciler kind", v1beta2.ProviderKind,
				"name", providerName.Name,
				"namespace", providerName.Namespace)
			return "", nil, true // cont calling loop
		}
	}

	if webhook == "" {
		s.logger.Error(nil, "provider has no address",
			"reconciler kind", v1beta2.ProviderKind,
			"name", providerName.Name,
			"namespace", providerName.Namespace)
		return "", nil, true // cont calling loop
	}

	factory := notifier.NewFactory(webhook, proxy, username, provider.Spec.Channel, token, headers, certPool, password)
	sender, err := factory.Notifier(provider.Spec.Type)
	if err != nil {
		s.logger.Error(err, "failed to initialize provider",
			"reconciler kind", v1beta2.ProviderKind,
			"name", providerName.Name,
			"namespace", providerName.Namespace)
		return "", nil, true // cont calling loop
	}
	return token, sender, false
}

// getCommitStatusesToDispatch gets the list of alert CRs from the system
func (s *EventServer) getCommitStatusesToDispatch(event *events.Event, ctx context.Context) ([]v1beta2.CommitStatus, error) {
	alerts := make([]v1beta2.CommitStatus, 0)

	var allCommitStatuses v1beta2.CommitStatusList
	err := s.kubeClient.List(ctx, &allCommitStatuses)
	if err != nil {
		s.logger.Error(err, "listing alerts failed")
		return alerts, err
	}

each_alert:
	for _, alert := range allCommitStatuses.Items {
		// skip suspended and not ready alerts
		isReady := conditions.IsReady(&alert)
		if alert.Spec.Suspend || !isReady {
			continue each_alert
		}

		// skip alert if the message matches a regex from the exclusion list
		if len(alert.Spec.ExclusionList) > 0 {
			for _, exp := range alert.Spec.ExclusionList {
				if r, err := regexp.Compile(exp); err == nil {
					if r.Match([]byte(event.Message)) {
						continue each_alert
					}
				} else {
					s.logger.Error(err, fmt.Sprintf("failed to compile regex: %s", exp))
				}
			}
		}

		// filter alerts by object and severity
		for _, source := range alert.Spec.EventSources {
			if source.Namespace == "" {
				source.Namespace = alert.Namespace
			}

			if s.eventMatchesCommitStatus(ctx, event, source, alert.Spec.EventSeverity) {
				alerts = append(alerts, alert)
			}
		}
	}
	return alerts, nil
}

func (s *EventServer) eventMatchesCommitStatus(ctx context.Context, event *events.Event, source v1beta2.CrossNamespaceObjectReference, severity string) bool {
	if event.InvolvedObject.Namespace == source.Namespace &&
		event.InvolvedObject.Kind == source.Kind {
		if event.Severity == severity ||
			severity == events.EventSeverityInfo {

			labelMatch := true
			if source.Name == "*" && source.MatchLabels != nil {
				var obj metav1.PartialObjectMetadata
				obj.SetGroupVersionKind(event.InvolvedObject.GroupVersionKind())
				obj.SetName(event.InvolvedObject.Name)
				obj.SetNamespace(event.InvolvedObject.Namespace)

				if err := s.kubeClient.Get(ctx, types.NamespacedName{
					Namespace: event.InvolvedObject.Namespace,
					Name:      event.InvolvedObject.Name,
				}, &obj); err != nil {
					s.logger.Error(err, "error getting object", "kind", event.InvolvedObject.Kind,
						"name", event.InvolvedObject.Name, "apiVersion", event.InvolvedObject.APIVersion)
				}

				sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
					MatchLabels: source.MatchLabels,
				})
				if err != nil {
					s.logger.Error(err, fmt.Sprintf("error using matchLabels from event source '%s'", source.Name))
				}

				labelMatch = sel.Matches(labels.Set(obj.GetLabels()))
			}

			if source.Name == "*" && labelMatch || event.InvolvedObject.Name == source.Name {
				return true
			}
		}
	}

	return false
}
