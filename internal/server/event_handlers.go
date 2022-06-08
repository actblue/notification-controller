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
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/fluxcd/notification-controller/internal/notifier"
	"github.com/fluxcd/pkg/runtime/events"
	"github.com/go-logr/logr"
)

func (s *EventServer) handleEvent() func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		s.logger.Info("handleEvent called ")
		event, err := s.getEventFromBody(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// find matching alerts
		alerts, err := s.getAlertsToDispatch(event, ctx)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// find matching alerts
		commitStatuses, err := s.getCommitStatusesToDispatch(event, ctx)
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

			token, sender, done := s.getNotifierForAlerts(alert, err, ctx)
			if done {
				return
			}

			notification := prepareNotificationEventForAlerts(event, alert)

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

			token, sender, done := s.getNotifierForCommitStatuses(cs, err, ctx)
			if done {
				return
			}

			notification := prepareNotificationEventForCommitStatuses(event, cs, s.logger.Error)

			go s.dispatchNotification(sender, notification, token, "commitstatus")
		}

		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *EventServer) getEventFromBody(r *http.Request) (*events.Event, error) {
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
		err = redactTokenFromError(err, token, s.logger)

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

func redactTokenFromError(err error, token string, log logr.Logger) error {
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
		// For backwards compatibility, include the revision without a group prefix
		revisionKey := "revision"
		if rev, ok := event.Metadata[revisionKey]; ok {
			meta[revisionKey] = rev
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

func prepareNotificationEventForAlerts(event *events.Event, alert v1beta1.Alert) events.Event {
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

func prepareNotificationEventForCommitStatuses(event *events.Event, cs v1beta2.CommitStatus, logError ErrorF) events.Event {
	notification := *event.DeepCopy()

	if notification.Metadata == nil {
		notification.Metadata = map[string]string{}
	}

	x := cs.Spec.Parameters

	var params map[string]interface{}

	params["CommitStatus"] = cs
	params["Event"] = event
	// TODO:  add in referenes from e.g. configmaps here

	// TODO:  rename CommitStatus.Parameters back to Template and add in a Parameters field

	notification.Metadata[notifier.Key] = expand(x.Key, params, logError)
	notification.Metadata[notifier.Description] = expand(x.Description, params, logError)
	notification.Metadata[notifier.TargetUrl] = expand(x.TargetURL, params, logError)

	notification.Metadata["summary"] = "Hello Commit Status!"
	return notification
}

type ErrorF func(err error, msg string, keysAndValues ...interface{})

// TODO:  the reconciler should parse and validate all templates
// expand expands a string as a golang text template and returns the result.
func expand(temp string, data map[string]interface{}, logError ErrorF) string {
	if kt, err := template.New("").Parse(temp); err != nil {
		buf := bytes.NewBufferString("")
		if err = kt.Execute(buf, data); err == nil {
			return buf.String()
		} else {
			logError(err, "Error executing template", "template", temp, "params", data)
		}
	} else {
		logError(err, "Error parsing template", "template", temp)
	}

	return temp
}
