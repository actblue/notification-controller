/*
Copyright 2020 The Flux authors

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

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/fluxcd/notification-controller/api/v1beta1"
	"github.com/fluxcd/notification-controller/api/v1beta2"
	"io"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/fluxcd/notification-controller/internal/notifier"
	"github.com/fluxcd/pkg/runtime/events"
)

const (
	// RevisionKey For backwards compatibility, include the revision without a group prefix
	RevisionKey string = "revision"
)

func (s *EventServer) handleEvent() func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		s.logger.Info("handleEvent called ")
		event, err := s.constructEventFromRequestBody(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// find matching alerts
		alerts, err := s.findAlertsToDispatch(event, ctx)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// find matching alerts
		commitStatuses, err := s.findCommitStatusesToDispatch(event, ctx)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if len(alerts) == 0 && len(commitStatuses) == 0 {
			s.logger.Info("Discarding event, no alerts found for the involved object",
				"reconciler kind", event.InvolvedObject.Kind,
				"name", event.InvolvedObject.Name,
				"namespace", event.InvolvedObject.Namespace)
			w.WriteHeader(http.StatusAccepted)
			return
		}

		s.logger.Info(fmt.Sprintf("Dispatching event: %s", event.Message),
			"reconciler kind", event.InvolvedObject.Kind,
			"name", event.InvolvedObject.Name,
			"namespace", event.InvolvedObject.Namespace)

		// dispatch notifications
		for _, alert := range alerts {
			// verify if event comes from a different namespace
			if s.noCrossNamespaceRefs && event.InvolvedObject.Namespace != alert.Namespace {
				accessDenied := fmt.Errorf(
					"alert '%s/%s' can't process event from '%s/%s/%s', cross-namespace references have been blocked",
					alert.Namespace, alert.Name, event.InvolvedObject.Kind, event.InvolvedObject.Namespace, event.InvolvedObject.Name)
				s.logger.Error(accessDenied, "Discarding event, access denied to cross-namespace sources")
				continue
			}

			token, sender, done := s.constructNotifierForAlerts(alert, err, ctx)
			if done {
				return
			}

			notification := constructNotificationEventForAlerts(event, alert)

			go s.dispatchNotification(sender, notification, token, "alert")
		}

		// dispatch notifications
		for _, cs := range commitStatuses {
			// verify if event comes from a different namespace
			if s.noCrossNamespaceRefs && event.InvolvedObject.Namespace != cs.Namespace {
				accessDenied := fmt.Errorf(
					"alert '%s/%s' can't process event from '%s/%s/%s', cross-namespace references have been blocked",
					cs.Namespace, cs.Name, event.InvolvedObject.Kind, event.InvolvedObject.Namespace, event.InvolvedObject.Name)
				s.logger.Error(accessDenied, "Discarding event, access denied to cross-namespace sources")
				continue
			}

			token, sender, done := s.constructNotifierForCommitStatuses(cs, err, ctx)
			if done {
				return
			}

			var notification events.Event
			notification, err = s.constructNotificationEventForCommitStatuses(ctx, event, cs)
			if err != nil {
				s.logger.Error(err, "")
			}

			go s.dispatchNotification(sender, notification, token, "commitstatus")
		}

		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *EventServer) constructEventFromRequestBody(r *http.Request) (*events.Event, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Error(err, "reading the request body failed")
		return nil, err
	}
	defer r.Body.Close()

	event := &events.Event{}
	err = json.Unmarshal(body, event)
	if err != nil {
		s.logger.Error(err, "decoding the request body failed")
		return nil, err
	}

	cleanupMetadata(event)
	return event, nil
}

func (s *EventServer) dispatchNotification(n notifier.Interface, e events.Event, token string, source string) {
	if err := n.Post(e, s.logger); err != nil {
		err = redactTokenFromError(err, token)

		s.logger.Error(err, "failed to send notification",
			"reconciler kind", e.InvolvedObject.Kind,
			"name", e.InvolvedObject.Name,
			"namespace", e.InvolvedObject.Namespace)
	} else {
		s.logger.Info("sent notification",
			"reconciler kind", e.InvolvedObject.Kind,
			"name", e.InvolvedObject.Name,
			"namespace", e.InvolvedObject.Namespace,
			"provider", reflect.TypeOf(n).Name(),
			"source", source,
		)
	}
}

func redactTokenFromError(err error, token string) error {
	if token == "" {
		return err
	}

	re, compileErr := regexp.Compile(fmt.Sprintf("%s*", regexp.QuoteMeta(token)))
	if compileErr != nil {
		newErrStr := fmt.Sprintf("error redacting token from error message: %s", compileErr)
		return errors.New(newErrStr)
	}

	redacted := re.ReplaceAllString(err.Error(), "*****")

	return errors.New(redacted)
}

// TODO: move the metadata filtering function to fluxcd/pkg/runtime/events
// cleanupMetadata removes metadata entries which are not used for alerting
func cleanupMetadata(event *events.Event) {
	group := event.InvolvedObject.GetObjectKind().GroupVersionKind().Group
	excludeList := []string{fmt.Sprintf("%s/checksum", group)}

	meta := make(map[string]string)

	if event.Metadata != nil && len(event.Metadata) > 0 {
		if rev, ok := event.Metadata[RevisionKey]; ok {
			meta[RevisionKey] = rev
		}

		// Filter other meta based on group prefix, while filtering out excludes
		for key, val := range event.Metadata {
			if strings.HasPrefix(key, group) && !inList(excludeList, key) {
				newKey := strings.TrimPrefix(key, fmt.Sprintf("%s/", group))
				meta[newKey] = val
			}
		}
	}

	event.Metadata = meta
}

func inList(l []string, i string) bool {
	for _, v := range l {
		if strings.EqualFold(v, i) {
			return true
		}
	}
	return false
}

func constructNotificationEventForAlerts(event *events.Event, alert v1beta1.Alert) events.Event {
	notification := *event.DeepCopy()
	if alert.Spec.Summary != "" {
		if notification.Metadata == nil {
			notification.Metadata = map[string]string{
				"summary": alert.Spec.Summary,
			}
		} else {
			notification.Metadata["summary"] = alert.Spec.Summary
		}
	}
	return notification
}

func (s *EventServer) constructNotificationEventForCommitStatuses(ctx context.Context, event *events.Event, cs v1beta2.CommitStatus) (events.Event, error) {
	notification := *event.DeepCopy()

	if notification.Metadata == nil {
		notification.Metadata = map[string]string{}
	}

	tplate := cs.Spec.Template
	templateParams, err := s.retrieveTemplateParams(ctx, event, cs)
	if err != nil {
		return notification, err
	}
	notification.Metadata[notifier.Key], err = s.expandTemplate(tplate.Key, templateParams)
	if err != nil {
		return notification, err
	}
	notification.Metadata[notifier.Description], err = s.expandTemplate(tplate.Description, templateParams)
	if err != nil {
		return notification, err
	}
	notification.Metadata[notifier.TargetUrl], err = s.expandTemplate(tplate.TargetURL, templateParams)
	if err != nil {
		return notification, err
	}

	return notification, nil
}

// todo: only run this when the revision date of the configmap objects are newer than the saved revision date of the last time we retrieved data from them?
// is that^ even necessary?
func (s *EventServer) retrieveTemplateParams(ctx context.Context, event *events.Event, cs v1beta2.CommitStatus) (map[string]interface{}, error) {
	templateParams := make(map[string]interface{})
	templateParams[RevisionKey] = event.Metadata[RevisionKey]
	templateParams["InvolvedObject"] = event.InvolvedObject

	for _, cmRef := range cs.Spec.Template.AdditionalParameters {
		var cm corev1.ConfigMap
		var namespace string
		if cmRef.Namespace == "" {
			namespace = cs.Namespace
		} else {
			namespace = cmRef.Namespace
		}
		err := s.kubeClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cmRef.Name}, &cm)
		if err != nil {
			s.logger.Error(err, fmt.Sprintf("Error getting configmap %s in namespace %s", cmRef.Name, cmRef.Namespace))
			return nil, err
		}
		if cm.Data != nil {
			for k, v := range cm.Data {
				s.logger.Info(cmRef.Name + "_" + k)
				templateParams[cmRef.Name+"_"+k] = v
			}
		}
	}

	return templateParams, nil
}

// TODO: the reconciler should parse and validate all templates. but how if this requires data from the event?
// expandTemplate expands a string as a golang text template and returns the result.
func (s *EventServer) expandTemplate(tplate string, params map[string]interface{}) (string, error) {
	var renderedTemplate string
	kt, err := template.New("").Parse(tplate)
	if err == nil {
		buf := bytes.NewBufferString("")
		if err = kt.Execute(buf, params); err != nil {
			s.logger.Error(err, "Error executing template", "template", tplate, "params", params)
			return "", err
		}
		renderedTemplate = buf.String()
	} else {
		s.logger.Error(err, "Error parsing template", "template", tplate)
		return "", err
	}

	return renderedTemplate, nil
}
