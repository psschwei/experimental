/*
Copyright 2019-2020 The Tekton Authors
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

package endpoints

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"

	"math/rand"

	restful "github.com/emicklei/go-restful"
	routesv1 "github.com/openshift/api/route/v1"
	logging "github.com/tektoncd/experimental/webhooks-extension/pkg/logging"
	"github.com/tektoncd/experimental/webhooks-extension/pkg/utils"
	pipelinesv1alpha1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	v1alpha1 "github.com/tektoncd/triggers/pkg/apis/triggers/v1alpha1"
	certv1beta1 "k8s.io/api/certificates/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/certificate/csr"

	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	modifyingEventListenerLock sync.Mutex
	actions                    = pipelinesv1alpha1.Param{Name: "Wext-Incoming-Actions", Value: pipelinesv1alpha1.ArrayOrString{Type: pipelinesv1alpha1.ParamTypeString, StringVal: "opened,reopened,synchronize"}}
)

const (
	eventListenerName  = "tekton-webhooks-eventlistener"
	routeName          = "el-" + eventListenerName
	webhookextPullTask = "monitor-task"
)

/*
	Creation of the eventlistener, called when no eventlistener exists at
	the point of webhook creation.
*/
func (r Resource) createEventListener(webhook webhook, namespace, monitorTriggerNamePrefix string) (*v1alpha1.EventListener, error) {

	monitorBindingName, err := r.getMonitorBindingName(webhook.GitRepositoryURL, webhook.PullTask)
	if err != nil {
		return nil, err
	}

	hookExtBinding, monitorExtBinding, err := r.createBindings(webhook, monitorBindingName, true)
	if err != nil {
		bindings := []string{hookExtBinding, monitorExtBinding}
		for _, binding := range bindings {
			if binding != "" {
				r.TriggersClient.TriggersV1alpha1().TriggerBindings(namespace).Delete(binding, &metav1.DeleteOptions{})
			}
		}
		return nil, err
	}

	pushTrigger := r.newTrigger(webhook.Name+"-"+webhook.Namespace+"-push-event",
		webhook.Pipeline+"-push-binding",
		webhook.Pipeline+"-template",
		webhook.GitRepositoryURL,
		"push, Push Hook, Tag Push Hook",
		webhook.AccessTokenRef,
		hookExtBinding)

	pullRequestTrigger := r.newTrigger(webhook.Name+"-"+webhook.Namespace+"-pullrequest-event",
		webhook.Pipeline+"-pullrequest-binding",
		webhook.Pipeline+"-template",
		webhook.GitRepositoryURL,
		"pull_request, Merge Request Hook",
		webhook.AccessTokenRef,
		hookExtBinding)

	// slightly dodgy code here as I take the first Interceptor,
	// but we dont currently let users add extra interceptors
	// note that this [0] pattern happens in multiple places
	pullRequestTrigger.Interceptors[0].Webhook.Header = append(pullRequestTrigger.Interceptors[0].Webhook.Header, actions)

	monitorTriggerName := r.generateMonitorTriggerName(monitorTriggerNamePrefix, []v1alpha1.EventListenerTrigger{})
	monitorTrigger := r.newTrigger(monitorTriggerName,
		monitorBindingName,
		webhook.PullTask+"-template",
		webhook.GitRepositoryURL,
		"pull_request, Merge Request Hook",
		webhook.AccessTokenRef,
		monitorExtBinding)
	monitorTrigger.Interceptors[0].Webhook.Header = append(monitorTrigger.Interceptors[0].Webhook.Header, actions)

	triggers := []v1alpha1.EventListenerTrigger{pushTrigger, pullRequestTrigger, monitorTrigger}

	eventListener := v1alpha1.EventListener{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eventListenerName,
			Namespace: namespace,
		},
		Spec: v1alpha1.EventListenerSpec{
			ServiceAccountName: "tekton-webhooks-extension-eventlistener",
			Triggers:           triggers,
		},
	}
	return r.TriggersClient.TriggersV1alpha1().EventListeners(namespace).Create(&eventListener)
}

/*
	Update of the eventlistener, called when adding additional webhooks as we
	run with a single eventlistener.
*/
func (r Resource) updateEventListener(eventListener *v1alpha1.EventListener, webhook webhook, monitorTriggerNamePrefix string) (*v1alpha1.EventListener, error) {

	createMonitorBinding := false
	monitorBindingName, err := r.getMonitorBindingName(webhook.GitRepositoryURL, webhook.PullTask)
	if err != nil {
		return nil, err
	}

	existingMonitorFound, _ := r.doesMonitorExist(monitorTriggerNamePrefix, webhook, eventListener.Spec.Triggers)
	if !existingMonitorFound {
		createMonitorBinding = true
	}

	hookExtBinding, monitorExtBinding, err := r.createBindings(webhook, monitorBindingName, createMonitorBinding)
	if err != nil {
		bindings := []string{hookExtBinding, monitorExtBinding}
		for _, binding := range bindings {
			if binding != "" {
				r.TriggersClient.TriggersV1alpha1().TriggerBindings(r.Defaults.Namespace).Delete(binding, &metav1.DeleteOptions{})
			}
		}
		return nil, err
	}

	newPushTrigger := r.newTrigger(webhook.Name+"-"+webhook.Namespace+"-push-event",
		webhook.Pipeline+"-push-binding",
		webhook.Pipeline+"-template",
		webhook.GitRepositoryURL,
		"push, Push Hook, Tag Push Hook",
		webhook.AccessTokenRef,
		hookExtBinding)

	newPullRequestTrigger := r.newTrigger(webhook.Name+"-"+webhook.Namespace+"-pullrequest-event",
		webhook.Pipeline+"-pullrequest-binding",
		webhook.Pipeline+"-template",
		webhook.GitRepositoryURL,
		"pull_request, Merge Request Hook",
		webhook.AccessTokenRef,
		hookExtBinding)
	newPullRequestTrigger.Interceptors[0].Webhook.Header = append(newPullRequestTrigger.Interceptors[0].Webhook.Header, actions)

	eventListener.Spec.Triggers = append(eventListener.Spec.Triggers, newPushTrigger)
	eventListener.Spec.Triggers = append(eventListener.Spec.Triggers, newPullRequestTrigger)

	if !existingMonitorFound {
		monitorTriggerName := r.generateMonitorTriggerName(monitorTriggerNamePrefix, eventListener.Spec.Triggers)
		newMonitor := r.newTrigger(monitorTriggerName,
			monitorBindingName,
			webhook.PullTask+"-template",
			webhook.GitRepositoryURL,
			"pull_request, Merge Request Hook",
			webhook.AccessTokenRef,
			monitorExtBinding)
		newMonitor.Interceptors[0].Webhook.Header = append(newMonitor.Interceptors[0].Webhook.Header, actions)

		eventListener.Spec.Triggers = append(eventListener.Spec.Triggers, newMonitor)
	}

	return r.TriggersClient.TriggersV1alpha1().EventListeners(eventListener.Namespace).Update(eventListener)
}

func (r Resource) compareGitRepoNames(url1, url2 string) (bool, error) {
	serverName1, ownerName1, repoName1, err1 := r.getGitValues(url1)
	serverName2, ownerName2, repoName2, err2 := r.getGitValues(url2)
	if err1 != nil {
		return false, err1
	}
	if err2 != nil {
		return false, err2
	}
	if serverName1 == serverName2 && ownerName1 == ownerName2 && repoName1 == repoName2 {
		return true, nil
	}
	return false, nil
}

func (r Resource) generateMonitorTriggerName(prefix string, existingTriggers []v1alpha1.EventListenerTrigger) string {
	rand.Seed(time.Now().UnixNano())
	suggestedName := prefix + strconv.Itoa(rand.Intn(10000))

	for {
		nameOK := true
		for _, t := range existingTriggers {
			if t.Name == suggestedName {
				nameOK = false
				suggestedName = prefix + strconv.Itoa(rand.Intn(10000))
				break
			}
		}
		if nameOK {
			break
		}
	}
	return suggestedName
}

func (r Resource) doesMonitorExist(monitorTriggerNamePrefix string, webhook webhook, triggers []v1alpha1.EventListenerTrigger) (bool, string) {
	existingMonitorFound := false
	monitorName := ""
	for _, trigger := range triggers {
		if strings.HasPrefix(trigger.Name, monitorTriggerNamePrefix) {
			// check to see if the trigger is for this webhook by checking repo URLs match
			// do by checking the Wext-Repository-Url on the trigger's interceptor params
			headers := trigger.Interceptors[0].Webhook.Header
			for _, header := range headers {
				if header.Name == "Wext-Repository-Url" {
					match, err := r.compareGitRepoNames(header.Value.StringVal, webhook.GitRepositoryURL)
					if err != nil {
						return false, ""
					}
					if match {
						existingMonitorFound = true
						monitorName = trigger.Name
						break
					}
				}
			}
			if existingMonitorFound {
				break
			}
		}
	}
	logging.Log.Debugf("does monitor exist for repo %s: %s", webhook.GitRepositoryURL, existingMonitorFound)
	return existingMonitorFound, monitorName
}

func (r Resource) getMonitorBindingName(repoURL, monitorTask string) (string, error) {
	logging.Log.Debugf("monitor task name is: %s", monitorTask)
	if monitorTask == "" {
		monitorTask = "monitor-task"
		logging.Log.Debugf("no monitor task specified, assuming name is %s", monitorTask)
	}

	monitorBindingName := monitorTask + "-binding"
	if monitorTask == webhookextPullTask {
		provider, _, err := utils.GetGitProviderAndAPIURL(repoURL)
		if err != nil {
			return "", err
		}
		monitorBindingName = monitorTask + "-" + provider + "-binding"
	}
	return monitorBindingName, nil
}

func (r Resource) newTrigger(name, bindingName, templateName, repoURL, event, secretName, extraBindingName string) v1alpha1.EventListenerTrigger {
	return v1alpha1.EventListenerTrigger{
		Name: name,
		Bindings: []*v1alpha1.EventListenerBinding{
			{
				Ref:        bindingName,
				APIVersion: "v1alpha1",
			},
			{
				Ref:        extraBindingName,
				APIVersion: "v1alpha1",
			},
		},
		Template: v1alpha1.EventListenerTemplate{
			Name:       templateName,
			APIVersion: "v1alpha1",
		},
		Interceptors: []*v1alpha1.EventInterceptor{
			{
				Webhook: &v1alpha1.WebhookInterceptor{
					Header: []pipelinesv1alpha1.Param{
						{Name: "Wext-Trigger-Name", Value: pipelinesv1alpha1.ArrayOrString{Type: pipelinesv1alpha1.ParamTypeString, StringVal: name}},
						{Name: "Wext-Repository-Url", Value: pipelinesv1alpha1.ArrayOrString{Type: pipelinesv1alpha1.ParamTypeString, StringVal: repoURL}},
						{Name: "Wext-Incoming-Event", Value: pipelinesv1alpha1.ArrayOrString{Type: pipelinesv1alpha1.ParamTypeString, StringVal: event}},
						{Name: "Wext-Secret-Name", Value: pipelinesv1alpha1.ArrayOrString{Type: pipelinesv1alpha1.ParamTypeString, StringVal: secretName}}},
					ObjectRef: &corev1.ObjectReference{
						APIVersion: "v1",
						Kind:       "Service",
						Name:       "tekton-webhooks-extension-validator",
						Namespace:  r.Defaults.Namespace,
					},
				},
			},
		},
	}
}

func (r Resource) getParams(webhook webhook) (webhookParams, monitorParams []v1alpha1.Param) {
	saName := webhook.ServiceAccount
	requestedReleaseName := webhook.ReleaseName
	if saName == "" {
		saName = "default"
	}
	server, org, repo, err := r.getGitValues(webhook.GitRepositoryURL)
	if err != nil {
		logging.Log.Errorf("error returned from getGitValues: %s", err)
	}
	server = strings.TrimPrefix(server, "https://")
	server = strings.TrimPrefix(server, "http://")

	releaseName := ""
	if requestedReleaseName != "" {
		logging.Log.Infof("Release name based on input: %s", requestedReleaseName)
		releaseName = requestedReleaseName
	} else {
		releaseName = repo
		logging.Log.Infof("Release name based on repository name: %s", releaseName)
	}

	sslVerify := true
	ssl := os.Getenv("SSL_VERIFICATION_ENABLED")
	if strings.ToLower(ssl) == "false" {
		logging.Log.Warn("SSL_VERIFICATION_ENABLED SET TO FALSE")
		sslVerify = false
	}

	provider, apiURL, err := utils.GetGitProviderAndAPIURL(webhook.GitRepositoryURL)
	if err != nil {
		logging.Log.Errorf("error returned from GetGitProviderAndAPIURL: %s", err)
	}

	hookParams := []v1alpha1.Param{
		{Name: "webhooks-tekton-release-name", Value: releaseName},
		{Name: "webhooks-tekton-target-namespace", Value: webhook.Namespace},
		{Name: "webhooks-tekton-service-account", Value: webhook.ServiceAccount},
		{Name: "webhooks-tekton-git-server", Value: server},
		{Name: "webhooks-tekton-git-org", Value: org},
		{Name: "webhooks-tekton-git-repo", Value: repo},
		{Name: "webhooks-tekton-pull-task", Value: webhook.PullTask},
		{Name: "webhooks-tekton-ssl-verify", Value: strconv.FormatBool(sslVerify)},
		{Name: "webhooks-tekton-insecure-skip-tls-verify", Value: strconv.FormatBool(!sslVerify)},
	}

	if webhook.DockerRegistry != "" {
		hookParams = append(hookParams, v1alpha1.Param{Name: "webhooks-tekton-docker-registry", Value: webhook.DockerRegistry})
	}
	if webhook.HelmSecret != "" {
		hookParams = append(hookParams, v1alpha1.Param{Name: "webhooks-tekton-helm-secret", Value: webhook.HelmSecret})
	}

	onSuccessComment := webhook.OnSuccessComment
	if onSuccessComment == "" {
		onSuccessComment = "Success"
	}
	onFailureComment := webhook.OnFailureComment
	if onFailureComment == "" {
		onFailureComment = "Failed"
	}
	onTimeoutComment := webhook.OnTimeoutComment
	if onTimeoutComment == "" {
		onTimeoutComment = "Unknown"
	}
	onMissingComment := webhook.OnMissingComment
	if onMissingComment == "" {
		onMissingComment = "Missing"
	}

	prMonitorParams := []v1alpha1.Param{
		{Name: "commentsuccess", Value: onSuccessComment},
		{Name: "commentfailure", Value: onFailureComment},
		{Name: "commenttimeout", Value: onTimeoutComment},
		{Name: "commentmissing", Value: onMissingComment},
		{Name: "gitsecretname", Value: webhook.AccessTokenRef},
		{Name: "gitsecretkeyname", Value: "accessToken"},
		{Name: "dashboardurl", Value: r.getDashboardURL(r.Defaults.Namespace)},
		{Name: "insecure-skip-tls-verify", Value: strconv.FormatBool(!sslVerify)},
		{Name: "provider", Value: provider},
		{Name: "apiurl", Value: apiURL},
	}

	return hookParams, prMonitorParams
}

// This is deliberately written as a function such that unittests can override
// and set the name of artifacts for creation due to limitation of k8s GenerateName
var GetTriggerBindingObjectMeta = func(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		GenerateName: "wext-" + name + "-",
	}
}

func (r Resource) createBindings(webhook webhook, monitorTriggerName string, createMonitorBinding bool) (webhookParamsBinding, monitorParamsBinding string, err error) {
	hookParams, prMonitorParams := r.getParams(webhook)
	hookBinding := v1alpha1.TriggerBinding{
		ObjectMeta: GetTriggerBindingObjectMeta(webhook.Name),
		Spec: v1alpha1.TriggerBindingSpec{
			Params: hookParams,
		},
	}
	actualHookBinding, err := r.TriggersClient.TriggersV1alpha1().TriggerBindings(r.Defaults.Namespace).Create(&hookBinding)
	if err != nil {
		logging.Log.Errorf("failed to create binding %+v, with error %s", hookBinding, err.Error())
		return "", "", err
	}

	if createMonitorBinding {
		monitorBinding := v1alpha1.TriggerBinding{
			ObjectMeta: GetTriggerBindingObjectMeta(monitorTriggerName),
			Spec: v1alpha1.TriggerBindingSpec{
				Params: prMonitorParams,
			},
		}

		actualMonitorBinding, err := r.TriggersClient.TriggersV1alpha1().TriggerBindings(r.Defaults.Namespace).Create(&monitorBinding)
		if err != nil {
			logging.Log.Errorf("failed to create binding %+v, with error %s", monitorBinding, err.Error())
			return actualHookBinding.Name, "", err
		}
		return actualHookBinding.Name, actualMonitorBinding.Name, nil
	} else {
		return actualHookBinding.Name, "", nil
	}

}

func (r Resource) getDashboardURL(installNs string) string {
	type element struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}

	toReturn := "http://localhost:9097/"

	// TODO: app.kubernetes.io/instance should be configurable (in case of multiple deployments)
	labelLookup := "app.kubernetes.io/part-of=tekton-dashboard,app.kubernetes.io/component=dashboard,app.kubernetes.io/name=dashboard"

	services, err := r.K8sClient.CoreV1().Services(installNs).List(metav1.ListOptions{LabelSelector: labelLookup})
	if err != nil {
		logging.Log.Errorf("could not find the dashboard's service - error: %s", err.Error())
		return toReturn
	}

	if len(services.Items) == 0 {
		logging.Log.Error("could not find the dashboard's service")
		return toReturn
	}

	name := services.Items[0].Name
	proto := services.Items[0].Spec.Ports[0].Name
	port := services.Items[0].Spec.Ports[0].Port
	url := fmt.Sprintf("%s://%s:%d/v1/namespaces/%s/endpoints", proto, name, port, installNs)
	logging.Log.Debugf("using url: %s", url)
	resp, err := http.DefaultClient.Get(url)
	if err != nil {
		logging.Log.Errorf("error occurred when hitting the endpoints REST endpoint: %s", err.Error())
		return url
	}
	if resp.StatusCode != 200 {
		logging.Log.Errorf("return code was not 200 when hitting the endpoints REST endpoint, code returned was: %d", resp.StatusCode)
		return url
	}

	bodyJSON := []element{}
	json.NewDecoder(resp.Body).Decode(&bodyJSON)
	return bodyJSON[0].URL
}

/*
	Processes a git URL into component parts, all of which are lowercased
	to try and avoid problems matching strings.
*/
func (r Resource) getGitValues(url string) (gitServer, gitOwner, gitRepo string, err error) {
	repoURL := ""
	prefix := ""
	if url != "" {
		url = strings.ToLower(url)
		if strings.Contains(url, "https://") {
			repoURL = strings.TrimPrefix(url, "https://")
			prefix = "https://"
		} else {
			repoURL = strings.TrimPrefix(url, "http://")
			prefix = "http://"
		}
	}
	// example at this point: github.com/tektoncd/pipeline
	numSlashes := strings.Count(repoURL, "/")
	if numSlashes < 2 {
		return "", "", "", errors.New("URL didn't contain an owner and repository")
	}
	repoURL = strings.TrimSuffix(repoURL, "/")
	gitServer = prefix + repoURL[0:strings.Index(repoURL, "/")]
	gitOwner = repoURL[strings.Index(repoURL, "/")+1 : strings.LastIndex(repoURL, "/")]
	//need to cut off the .git
	if strings.HasSuffix(url, ".git") {
		gitRepo = repoURL[strings.LastIndex(repoURL, "/")+1 : len(repoURL)-4]
	} else {
		gitRepo = repoURL[strings.LastIndex(repoURL, "/")+1:]
	}

	return gitServer, gitOwner, gitRepo, nil
}

// Creates a webhook for a given repository and populates (creating if doesn't yet exist) an eventlistener
func (r Resource) createWebhook(request *restful.Request, response *restful.Response) {
	modifyingEventListenerLock.Lock()
	defer modifyingEventListenerLock.Unlock()

	logging.Log.Infof("Webhook creation request received with request: %+v.", request)
	installNs := r.Defaults.Namespace

	webhook := webhook{}
	if err := request.ReadEntity(&webhook); err != nil {
		logging.Log.Errorf("error trying to read request entity as webhook: %s.", err)
		RespondError(response, err, http.StatusBadRequest)
		return
	}

	// Sanitize GitRepositoryURL
	webhook.GitRepositoryURL = strings.TrimSuffix(webhook.GitRepositoryURL, ".git")

	if webhook.PullTask == "" {
		webhook.PullTask = webhookextPullTask
	}

	if webhook.Name != "" {
		if len(webhook.Name) > 57 {
			tooLongMessage := fmt.Sprintf("requested webhook name (%s) must be less than 58 characters", webhook.Name)
			err := errors.New(tooLongMessage)
			logging.Log.Errorf("error: %s", err.Error())
			RespondError(response, err, http.StatusBadRequest)
			return
		}
	}

	dockerRegDefault := r.Defaults.DockerRegistry
	// remove prefixes if any
	webhook.DockerRegistry = strings.TrimPrefix(webhook.DockerRegistry, "https://")
	webhook.DockerRegistry = strings.TrimPrefix(webhook.DockerRegistry, "http://")
	if webhook.DockerRegistry == "" && dockerRegDefault != "" {
		webhook.DockerRegistry = dockerRegDefault
	}
	logging.Log.Debugf("Docker registry location is: %s", webhook.DockerRegistry)

	namespace := webhook.Namespace
	if namespace == "" {
		err := errors.New("a namespace for creating a webhook is required, but none was given")
		logging.Log.Errorf("error: %s.", err.Error())
		RespondError(response, err, http.StatusBadRequest)
		return
	}

	if !strings.HasPrefix(webhook.GitRepositoryURL, "http") {
		err := errors.New("the supplied GitRepositoryURL does not specify the protocol http:// or https://")
		logging.Log.Errorf("error: %s", err.Error())
		RespondError(response, err, http.StatusBadRequest)
		return
	}

	pieces := strings.Split(webhook.GitRepositoryURL, "/")
	if len(pieces) < 4 {
		logging.Log.Errorf("error creating webhook: GitRepositoryURL format error (%+v).", webhook.GitRepositoryURL)
		RespondError(response, errors.New("GitRepositoryURL format error"), http.StatusBadRequest)
		return
	}

	hooks, err := r.getHooksForRepo(webhook.GitRepositoryURL)
	if len(hooks) > 0 {
		for _, hook := range hooks {

			if hook.Name == webhook.Name {
				logging.Log.Errorf("error creating webhook: A webhook already exists with this name: %s", webhook.Name)
				RespondError(response, errors.New("Webhook already exists with the same name"), http.StatusBadRequest)
				return
			}
			if hook.Pipeline == webhook.Pipeline && hook.Namespace == webhook.Namespace {
				logging.Log.Errorf("error creating webhook: A webhook already exists for GitRepositoryURL %+v, running pipeline %s in namespace %s.", webhook.GitRepositoryURL, webhook.Pipeline, webhook.Namespace)
				RespondError(response, errors.New("Webhook already exists for the specified Git repository, running the same pipeline in the same namespace"), http.StatusBadRequest)
				return
			}
			if hook.PullTask != webhook.PullTask {
				msg := fmt.Sprintf("PullTask mismatch. Webhooks on a repository must use the same PullTask existing webhooks use %s not %s.", hook.PullTask, webhook.PullTask)
				logging.Log.Errorf("error creating webhook: " + msg)
				RespondError(response, errors.New(msg), http.StatusBadRequest)
				return
			}
		}
	}

	_, templateErr := r.TriggersClient.TriggersV1alpha1().TriggerTemplates(installNs).Get(webhook.Pipeline+"-template", metav1.GetOptions{})
	_, pushErr := r.TriggersClient.TriggersV1alpha1().TriggerBindings(installNs).Get(webhook.Pipeline+"-push-binding", metav1.GetOptions{})
	_, pullrequestErr := r.TriggersClient.TriggersV1alpha1().TriggerBindings(installNs).Get(webhook.Pipeline+"-pullrequest-binding", metav1.GetOptions{})
	if templateErr != nil || pushErr != nil || pullrequestErr != nil {
		msg := fmt.Sprintf("Could not find the required trigger template or trigger bindings in namespace: %s. Expected to find: %s, %s and %s", installNs, webhook.Pipeline+"-template", webhook.Pipeline+"-push-binding", webhook.Pipeline+"-pullrequest-binding")
		logging.Log.Errorf("%s", msg)
		logging.Log.Errorf("template error: `%s`, pushbinding error: `%s`, pullrequest error: `%s`", templateErr, pushErr, pullrequestErr)
		RespondError(response, errors.New(msg), http.StatusBadRequest)
		return
	}

	eventListener, err := r.TriggersClient.TriggersV1alpha1().EventListeners(installNs).Get(eventListenerName, metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		msg := fmt.Sprintf("unable to create webhook due to error listing Tekton eventlistener: %s", err)
		logging.Log.Errorf("%s", msg)
		RespondError(response, errors.New(msg), http.StatusInternalServerError)
		return
	}

	gitServer, gitOwner, gitRepo, err := r.getGitValues(webhook.GitRepositoryURL)
	if err != nil {
		logging.Log.Errorf("error parsing git repository URL %s in getGitValues(): %s", webhook.GitRepositoryURL, err)
		RespondError(response, errors.New("error parsing GitRepositoryURL, check pod logs for more details"), http.StatusInternalServerError)
		return
	}
	sanitisedURL := gitServer + "/" + gitOwner + "/" + gitRepo
	// Single monitor trigger for all triggers on a repo - thus name to use for monitor is
	monitorTriggerNamePrefix := gitOwner + "." + gitRepo + "-"

	if eventListener != nil && eventListener.Name != "" {
		_, err := r.updateEventListener(eventListener, webhook, monitorTriggerNamePrefix)
		if err != nil {
			msg := fmt.Sprintf("error creating webhook due to error updating eventlistener: %s", err)
			logging.Log.Errorf("%s", msg)
			RespondError(response, errors.New(msg), http.StatusInternalServerError)
			return
		}
	} else {
		logging.Log.Info("No existing eventlistener found, creating a new one...")
		_, err := r.createEventListener(webhook, installNs, monitorTriggerNamePrefix)
		if err != nil {
			msg := fmt.Sprintf("error creating webhook due to error creating eventlistener. Error was: %s", err)
			logging.Log.Errorf("%s", msg)
			RespondError(response, errors.New(msg), http.StatusInternalServerError)
			return
		}

		_, varexists := os.LookupEnv("PLATFORM")
		if !varexists {
			err = r.createDeleteIngress("create", installNs)
			if err != nil {
				msg := fmt.Sprintf("error creating webhook due to error creating ingress. Error was: %s", err)
				logging.Log.Errorf("%s", msg)
				logging.Log.Debugf("Deleting eventlistener as failed creating Ingress")
				err2 := r.TriggersClient.TriggersV1alpha1().EventListeners(installNs).Delete(eventListenerName, &metav1.DeleteOptions{})
				if err2 != nil {
					updatedMsg := fmt.Sprintf("error creating webhook due to error creating ingress. Also failed to cleanup and delete eventlistener. Errors were: %s and %s", err, err2)
					RespondError(response, errors.New(updatedMsg), http.StatusInternalServerError)
					return
				}
				RespondError(response, errors.New(msg), http.StatusInternalServerError)
				return
			} else {
				logging.Log.Debug("ingress creation succeeded")
			}
		} else {
			if err := r.createOpenshiftRoute(routeName); err != nil {
				logging.Log.Debug("Failed to create Route, deleting EventListener...")
				err2 := r.TriggersClient.TriggersV1alpha1().EventListeners(installNs).Delete(eventListenerName, &metav1.DeleteOptions{})
				if err2 != nil {
					updatedMsg := fmt.Sprintf("Error creating webhook due to error creating route. Also failed to cleanup and delete eventlistener. Errors were: %s and %s", err, err2)
					RespondError(response, errors.New(updatedMsg), http.StatusInternalServerError)
					return
				}
				RespondError(response, err, http.StatusInternalServerError)
				return
			}
		}

	}

	if len(hooks) == 0 {
		// // Give the eventlistener a chance to be up and running or webhook ping
		// // will get a 503 and might confuse people (although resend will work)
		for i := 0; i < 30; i = i + 1 {
			a, _ := r.K8sClient.AppsV1beta1().Deployments(installNs).Get(routeName, metav1.GetOptions{})
			replicas := a.Status.ReadyReplicas
			if replicas > 0 {
				break
			}
			time.Sleep(1 * time.Second)
		}

		// Create webhook
		err = r.AddWebhook(webhook, gitOwner, gitRepo)
		if err != nil {
			err2 := r.deleteFromEventListener(webhook.Name+"-"+webhook.Namespace, installNs, monitorTriggerNamePrefix, webhook)
			if err2 != nil {
				updatedMsg := fmt.Sprintf("error creating webhook. Also failed to cleanup and delete entry from eventlistener. Errors were: %s and %s", err, err2)
				RespondError(response, errors.New(updatedMsg), http.StatusInternalServerError)
				return
			}
			RespondError(response, err, http.StatusInternalServerError)
			return
		}
		logging.Log.Debug("webhook creation succeeded")
	} else {
		logging.Log.Debugf("webhook already exists for repository %s - not creating new hook in GitHub", sanitisedURL)
	}

	response.WriteHeader(http.StatusCreated)
}

func (r Resource) createDeleteIngress(mode, installNS string) error {
	if mode == "create" {
		// Unlike webhook creation, the ingress does not need a protocol specified
		callback := strings.TrimPrefix(r.Defaults.CallbackURL, "http://")
		callback = strings.TrimPrefix(callback, "https://")

		ingress := &v1beta1.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "el-" + eventListenerName,
				Namespace: installNS,
			},
			Spec: v1beta1.IngressSpec{
				Rules: []v1beta1.IngressRule{
					{
						Host: callback,
						IngressRuleValue: v1beta1.IngressRuleValue{
							HTTP: &v1beta1.HTTPIngressRuleValue{
								Paths: []v1beta1.HTTPIngressPath{
									{
										Backend: v1beta1.IngressBackend{
											ServiceName: "el-" + eventListenerName,
											ServicePort: intstr.IntOrString{
												Type:   intstr.Int,
												IntVal: 8080,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}
		// Check if TLS should be added
		if strings.Index(r.Defaults.CallbackURL, "https://") == 0 {
			certSecret, exists := os.LookupEnv("WEBHOOK_TLS_CERTIFICATE")
			if !exists {
				certSecret = "cert-" + eventListenerName
			}
			// check if the secret exists
			_, err := r.K8sClient.CoreV1().Secrets(installNS).Get(certSecret, metav1.GetOptions{})
			if err != nil {
				// create certificate
				certSecret = r.createCertificate(certSecret, installNS, callback)
			}
			if certSecret != "" {
				// add TLS in the IngressSpec
				ingressTLS := v1beta1.IngressTLS{
					Hosts:      []string{callback},
					SecretName: certSecret,
				}
				ingress.Spec.TLS = append(ingress.Spec.TLS, ingressTLS)
			} else {
				logging.Log.Error("Failed enabling TLS")
			}
		}

		ingress, err := r.K8sClient.ExtensionsV1beta1().Ingresses(installNS).Create(ingress)
		if err != nil {
			return err
		}
		logging.Log.Debug("Ingress has been created")
		return nil
	} else if mode == "delete" {
		err := r.K8sClient.ExtensionsV1beta1().Ingresses(installNS).Delete("el-"+eventListenerName, &metav1.DeleteOptions{})
		if err != nil {
			return err
		}
		logging.Log.Debug("Ingress has been deleted")
		return nil
	} else {
		logging.Log.Debug("Wrong mode")
		return errors.New("Wrong mode for createDeleteIngress")
	}
}

// Removes from Eventlistener, removes the webhook
func (r Resource) deleteWebhook(request *restful.Request, response *restful.Response) {
	modifyingEventListenerLock.Lock()
	defer modifyingEventListenerLock.Unlock()
	logging.Log.Debug("In deleteWebhook")
	name := request.PathParameter("name")
	repo := request.QueryParameter("repository")
	namespace := request.QueryParameter("namespace")
	deletePipelineRuns := request.QueryParameter("deletepipelineruns")

	var toDeletePipelineRuns = false
	var err error

	if deletePipelineRuns != "" {
		toDeletePipelineRuns, err = strconv.ParseBool(deletePipelineRuns)
		if err != nil {
			theError := errors.New("bad request information provided, cannot handle deletepipelineruns query (should be set to true or not provided)")
			logging.Log.Error(theError)
			RespondError(response, theError, http.StatusInternalServerError)
			return
		}
	}

	if namespace == "" || repo == "" {
		theErrorMessage := fmt.Sprintf("bad request information provided, a namespace and a repository must be specified as query parameters. Namespace: %s, repo: %s", namespace, repo)
		theError := errors.New(theErrorMessage)
		logging.Log.Error(theError)
		RespondError(response, theError, http.StatusBadRequest)
		return
	}

	logging.Log.Debugf("in deleteWebhook, name: %s, repo: %s, delete pipeline runs: %s", name, repo, deletePipelineRuns)

	webhooks, err := r.getHooksForRepo(repo)
	if err != nil {
		RespondError(response, err, http.StatusNotFound)
		return
	}

	logging.Log.Debugf("Found %d webhooks/pipelines registered against repo %s", len(webhooks), repo)
	if len(webhooks) < 1 {
		err := fmt.Errorf("no webhook found for repo %s", repo)
		logging.Log.Error(err)
		RespondError(response, err, http.StatusBadRequest)
		return
	}

	_, gitOwner, gitRepo, err := r.getGitValues(repo)
	if err != nil {
		err := fmt.Errorf("error getting git values for repo %s", repo)
		logging.Log.Error(err)
		RespondError(response, err, http.StatusInternalServerError)
		return
	}
	// Single monitor trigger for all triggers on a repo - thus name to use for monitor is
	monitorTriggerNamePrefix := gitOwner + "." + gitRepo + "-"

	found := false
	for _, hook := range webhooks {
		if hook.Name == name && hook.Namespace == namespace {
			found = true
			if len(webhooks) == 1 {
				logging.Log.Debug("No other pipelines triggered by this GitHub webhook, deleting webhook")
				// Delete webhook
				logging.Log.Debugf("Removing hook %s, owner: %s, repo: %s", hook, gitOwner, gitRepo)
				err := r.RemoveWebhook(hook, gitOwner, gitRepo)
				if err != nil {
					logging.Log.Errorf("error removing webhook: %s", err)
					RespondError(response, err, http.StatusInternalServerError)
					return
				}
				logging.Log.Debug("Webhook deletion succeeded")
			}
			if toDeletePipelineRuns {
				r.deletePipelineRuns(repo, namespace, hook.Pipeline)
			}
			eventListenerEntryPrefix := name + "-" + namespace
			err = r.deleteFromEventListener(eventListenerEntryPrefix, r.Defaults.Namespace, monitorTriggerNamePrefix, hook)
			if err != nil {
				logging.Log.Error(err)
				theError := errors.New("error deleting webhook from eventlistener")
				RespondError(response, theError, http.StatusInternalServerError)
				return
			}

			response.WriteHeader(204)
		}
	}

	if !found {
		err := fmt.Errorf("no webhook found for repo %s with name %s associated with namespace %s", repo, name, namespace)
		logging.Log.Error(err)
		RespondError(response, err, http.StatusNotFound)
		return
	}

}

// create signed certificate and set it into secret
func (r Resource) createCertificate(secretName, installNS, callback string) string {
	var key, crt []byte

	priv, _ := rsa.GenerateKey(cryptorand.Reader, 2048)
	template := x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   callback,
			Country:      []string{"Country"},
			Province:     []string{"Province"},
			Organization: []string{"Organization"},
		},
	}
	csrdata, err := cert.MakeCSRFromTemplate(priv, &template)
	if err != nil {
		logging.Log.Errorf("Failed creating CSR data: %v", err)
		return ""
	}
	logging.Log.Debug(csrdata)
	client := r.K8sClient.CertificatesV1beta1().CertificateSigningRequests()
	csrRecord, err := csr.RequestCertificate(client, csrdata, secretName, []certv1beta1.KeyUsage{certv1beta1.UsageDigitalSignature, certv1beta1.UsageKeyEncipherment, certv1beta1.UsageServerAuth}, priv)
	if err != nil {
		logging.Log.Errorf("Failed creating CSR record: %v", err)
		return ""
	}

	// approve csr manually
	csrRecord, err = client.Get(secretName, metav1.GetOptions{})
	if err != nil {
		logging.Log.Errorf("Failed getting CSR record: %v", err)
		return ""
	}
	csrRecord.Status.Conditions = append(csrRecord.Status.Conditions, certv1beta1.CertificateSigningRequestCondition{
		Type:    certv1beta1.CertificateApproved,
		Reason:  "AutoApproved",
		Message: "Approved by Tekton webhook",
	})
	_, err = client.UpdateApproval(csrRecord)
	if err != nil {
		logging.Log.Errorf("Failed approving CSR: %v", err)
		return ""
	}

	// csrdata, err = csr.WaitForCertificate(client, csrRecord, time.Duration(3600*time.Second))
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(3600*time.Second))
	// Even though ctx will be expired, it is good practice to call its
	// cancellation function in any case. Failure to do so may keep the
	// context and its parent alive longer than necessary.
	defer cancel()
	csrdata, err = csr.WaitForCertificate(ctx, client, csrRecord)
	if err != nil {
		logging.Log.Errorf("Failed waiting for certificate: %v", err)
		return ""
	}

	// retrive signed certificate
	csrRecord, err = client.Get(secretName, metav1.GetOptions{})
	if err != nil {
		logging.Log.Errorf("Failed getting certificate: %v", err)
		return ""
	}
	crt = csrRecord.Status.Certificate

	var keyOut bytes.Buffer
	err = pem.Encode(&keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	if err != nil {
		logging.Log.Errorf("Failed encoding private key: %v", err)
		return ""
	}
	key = keyOut.Bytes()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: installNS,
		},
		Type: "kubernetes.io/tls",
		Data: map[string][]byte{
			"tls.crt": crt,
			"tls.key": key,
		},
	}
	_, err = r.K8sClient.CoreV1().Secrets(installNS).Create(secret)
	if err != nil {
		logging.Log.Error("Failed creating TLS secret: %v", err)
		return ""
	} else {
		return secretName
	}
}

func (r Resource) deleteFromEventListener(name, installNS, monitorTriggerNamePrefix string, webhook webhook) error {
	logging.Log.Debugf("Deleting triggers for %s from the eventlistener", name)
	el, err := r.TriggersClient.TriggersV1alpha1().EventListeners(installNS).Get(eventListenerName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	monitorBindingName, err := r.getMonitorBindingName(webhook.GitRepositoryURL, webhook.PullTask)
	if err != nil {
		return err
	}

	toRemove := []string{name + "-push-event", name + "-pullrequest-event"}
	// store bindings to remove in this map as dupes won't be added
	bindingsToRemove := make(map[string]string)

	var newTriggers []v1alpha1.EventListenerTrigger
	currentTriggers := el.Spec.Triggers

	var monitorTrigger v1alpha1.EventListenerTrigger
	actualMonitorBindingName := ""
	triggersOnRepo := 0
	triggersDeleted := 0

	existingMonitorFound, monitorTriggerName := r.doesMonitorExist(monitorTriggerNamePrefix, webhook, el.Spec.Triggers)

	for _, t := range currentTriggers {
		if existingMonitorFound && t.Name == monitorTriggerName {
			monitorTrigger = t
			for _, binding := range t.Bindings {
				if strings.HasPrefix(binding.Name, "wext-"+monitorBindingName+"-") {
					actualMonitorBindingName = binding.Name
				}
			}
		} else {
			// check to see if the trigger is for this webhook by checking repo URLs match
			// do by checking the Wext-Repository-Url on the trigger's interceptor param
			interceptorParams := t.Interceptors[0].Webhook.Header
			for _, p := range interceptorParams {
				if p.Name == "Wext-Repository-Url" && p.Value.StringVal == webhook.GitRepositoryURL {
					triggersOnRepo++
				}
			}
			found := false
			for _, triggerName := range toRemove {
				if triggerName == t.Name {
					triggersDeleted++
					found = true
					for _, binding := range t.Bindings {
						if strings.HasPrefix(binding.Name, "wext-"+webhook.Name+"-") {
							bindingsToRemove[binding.Name] = binding.Name
						}
					}
					break
				}
			}
			if !found {
				newTriggers = append(newTriggers, t)
			}
		}
	}

	if triggersOnRepo > triggersDeleted {
		// Leave the monitor entry
		newTriggers = append(newTriggers, monitorTrigger)
	} else {
		// OK to delete monitor binding as monitor getting deleted
		bindingsToRemove[actualMonitorBindingName] = actualMonitorBindingName
	}

	if len(newTriggers) == 0 {
		err = r.TriggersClient.TriggersV1alpha1().EventListeners(installNS).Delete(el.Name, &metav1.DeleteOptions{})
		if err != nil {
			return err
		}

		_, varExists := os.LookupEnv("PLATFORM")
		if !varExists {
			err = r.createDeleteIngress("delete", installNS)
			if err != nil {
				logging.Log.Errorf("error deleting ingress: %s", err)
				return err
			} else {
				logging.Log.Debug("Ingress deleted")
			}
		} else {
			if err := r.deleteOpenshiftRoute(routeName); err != nil {
				msg := fmt.Sprintf("error deleting webhook due to error deleting route. Error was: %s", err)
				logging.Log.Errorf("%s", msg)
				return err
			}
			logging.Log.Debug("route deletion succeeded")
		}
	} else {
		el.Spec.Triggers = newTriggers
		logging.Log.Debugf("Update eventlistener: %+v", el.Spec.Triggers)
		_, err = r.TriggersClient.TriggersV1alpha1().EventListeners(installNS).Update(el)
		if err != nil {
			logging.Log.Errorf("error updating eventlistener: %s", err)
			return err
		}
	}

	for binding := range bindingsToRemove {
		err = r.TriggersClient.TriggersV1alpha1().TriggerBindings(installNS).Delete(binding, &metav1.DeleteOptions{})
		if err != nil {
			logging.Log.Errorf("error deleting triggerbinding: %s", binding)
			logging.Log.Errorf("error: %s", err)
		}
	}
	return err
}

func (r Resource) getAllWebhooks(request *restful.Request, response *restful.Response) {
	logging.Log.Debugf("Get all webhooks")
	webhooks, err := r.getWebhooksFromEventListener()
	if err != nil {
		logging.Log.Errorf("error trying to get webhooks: %s.", err.Error())
		RespondError(response, err, http.StatusInternalServerError)
		return
	}
	response.WriteEntity(webhooks)
}

func (r Resource) getHooksForRepo(gitURL string) ([]webhook, error) {
	hooksForRepo := []webhook{}
	allHooks, err := r.getWebhooksFromEventListener()
	if err != nil {
		return nil, err
	}

	for _, hook := range allHooks {
		if hook.GitRepositoryURL == gitURL {
			hooksForRepo = append(hooksForRepo, hook)
		}
	}
	logging.Log.Debugf("hooks for repo %s: %s", gitURL, hooksForRepo)
	return hooksForRepo, nil
}

func (r Resource) getWebhooksFromEventListener() ([]webhook, error) {
	logging.Log.Debugf("Getting webhooks from eventlistener")
	el, err := r.TriggersClient.TriggersV1alpha1().EventListeners(r.Defaults.Namespace).Get(eventListenerName, metav1.GetOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return []webhook{}, nil
		}
		return nil, err
	}
	hooks := []webhook{}
	var hook webhook
	for _, trigger := range el.Spec.Triggers {
		checkHook := false
		if strings.HasSuffix(trigger.Name, "-push-event") {
			hook = r.getHookFromTrigger(trigger, "-push-event")
			checkHook = true
		} else if strings.HasSuffix(trigger.Name, "-pullrequest-event") {
			hook = r.getHookFromTrigger(trigger, "-pullrequest-event")
			checkHook = true
		}
		if checkHook && !containedInArray(hooks, hook) {
			hooks = append(hooks, hook)
		}
	}
	return hooks, nil
}

func (r Resource) getHookFromTrigger(t v1alpha1.EventListenerTrigger, suffix string) webhook {
	var releaseName, namespace, serviceaccount, pulltask, dockerreg, helmsecret, repo, gitSecret string
	for _, binding := range t.Bindings {
		b, err := r.TriggersClient.TriggersV1alpha1().TriggerBindings(r.Defaults.Namespace).Get(binding.Ref, metav1.GetOptions{})
		if err != nil {
			logging.Log.Errorf("Error retrieving webhook information in full - could not find required TriggerBinding %s", binding.Ref)
			t.Name = "Broken webhook! Resources not found"
		}
		for _, param := range b.Spec.Params {
			switch param.Name {
			case "webhooks-tekton-release-name":
				releaseName = param.Value
				logging.Log.Debugf("Thinking the webhook name is %s", releaseName)
			case "webhooks-tekton-target-namespace":
				namespace = param.Value
			case "webhooks-tekton-service-account":
				serviceaccount = param.Value
			case "webhooks-tekton-pull-task":
				pulltask = param.Value
			case "webhooks-tekton-docker-registry":
				dockerreg = param.Value
			case "webhooks-tekton-helm-secret":
				helmsecret = param.Value
			}
		}
	}

	// Interceptors now have a type (we are using Webhook), and there can
	// be multiple, as we only currently allow our interceptor we simply
	// take the first
	for _, header := range t.Interceptors[0].Webhook.Header {
		switch header.Name {
		case "Wext-Repository-Url":
			repo = header.Value.StringVal
		case "Wext-Secret-Name":
			gitSecret = header.Value.StringVal
		}
	}

	if namespace == "" {
		// For the broken webhook case - namespace and repo url is required for a successful delete
		namespace = r.Defaults.Namespace
	}

	// This data is what will be displayed via the UI
	triggerAsHook := webhook{
		Name:             strings.TrimSuffix(t.Name, "-"+namespace+suffix),
		Namespace:        namespace,
		Pipeline:         strings.TrimSuffix(t.Template.Name, "-template"),
		GitRepositoryURL: repo,
		HelmSecret:       helmsecret,
		PullTask:         pulltask,
		DockerRegistry:   dockerreg,
		ServiceAccount:   serviceaccount,
		ReleaseName:      releaseName,
		AccessTokenRef:   gitSecret,
	}

	return triggerAsHook
}

func containedInArray(array []webhook, hook webhook) bool {
	for _, item := range array {
		if item == hook {
			return true
		}
	}
	return false
}

func (r Resource) deletePipelineRuns(gitRepoURL, namespace, pipeline string) error {
	logging.Log.Debugf("Looking for PipelineRuns in namespace %s with repository URL %s for pipeline %s", namespace, gitRepoURL, pipeline)

	allPipelineRuns, err := r.TektonClient.TektonV1alpha1().PipelineRuns(namespace).List(metav1.ListOptions{})

	if err != nil {
		logging.Log.Errorf("Unable to retrieve PipelineRuns in the namespace %s! Error: %s", namespace, err.Error())
		return err
	}

	found := false
	for _, pipelineRun := range allPipelineRuns.Items {
		if pipelineRun.Spec.PipelineRef.Name == pipeline {
			labels := pipelineRun.Labels
			serverURL := labels["webhooks.tekton.dev/gitServer"]
			orgName := labels["webhooks.tekton.dev/gitOrg"]
			repoName := labels["webhooks.tekton.dev/gitRepo"]
			foundRepoURL := fmt.Sprintf("https://%s/%s/%s", serverURL, orgName, repoName)

			gitRepoURL = strings.ToLower(strings.TrimSuffix(gitRepoURL, ".git"))
			foundRepoURL = strings.ToLower(strings.TrimSuffix(foundRepoURL, ".git"))

			if foundRepoURL == gitRepoURL {
				found = true
				err := r.TektonClient.TektonV1alpha1().PipelineRuns(namespace).Delete(pipelineRun.Name, &metav1.DeleteOptions{})
				if err != nil {
					logging.Log.Errorf("failed to delete %s, error: %s", pipelineRun.Name, err.Error())
					return err
				}
				logging.Log.Infof("Deleted PipelineRun %s", pipelineRun.Name)
			}
		}
	}
	if !found {
		logging.Log.Infof("No matching PipelineRuns found")
	}
	return nil
}

func (r Resource) getDefaults(request *restful.Request, response *restful.Response) {
	logging.Log.Debugf("getDefaults returning: %v", r.Defaults)
	response.WriteEntity(r.Defaults)
}

// RespondError ...
func RespondError(response *restful.Response, err error, statusCode int) {
	logging.Log.Errorf("Error for RespondError: %s.", err.Error())
	logging.Log.Errorf("Response is %v.", *response)
	response.AddHeader("Content-Type", "text/plain")
	response.WriteError(statusCode, err)
}

// RespondErrorMessage ...
func RespondErrorMessage(response *restful.Response, message string, statusCode int) {
	logging.Log.Errorf("Message for RespondErrorMessage: %s.", message)
	response.AddHeader("Content-Type", "text/plain")
	response.WriteErrorString(statusCode, message)
}

// RespondErrorAndMessage ...
func RespondErrorAndMessage(response *restful.Response, err error, message string, statusCode int) {
	logging.Log.Errorf("Error for RespondErrorAndMessage: %s.", err.Error())
	logging.Log.Errorf("Message for RespondErrorAndMesage: %s.", message)
	response.AddHeader("Content-Type", "text/plain")
	response.WriteErrorString(statusCode, message)
}

// RegisterExtensionWebService registers the webhook webservice
func (r Resource) RegisterExtensionWebService(container *restful.Container) {
	ws := new(restful.WebService)
	ws.
		Path("/webhooks").
		Consumes(restful.MIME_JSON, restful.MIME_JSON).
		Produces(restful.MIME_JSON, restful.MIME_JSON)

	ws.Route(ws.POST("/").To(r.createWebhook))
	ws.Route(ws.GET("/").To(r.getAllWebhooks))
	ws.Route(ws.GET("/defaults").To(r.getDefaults))
	ws.Route(ws.DELETE("/{name}").To(r.deleteWebhook))

	ws.Route(ws.POST("/credentials").To(r.createCredential))
	ws.Route(ws.GET("/credentials").To(r.getAllCredentials))
	ws.Route(ws.DELETE("/credentials/{name}").To(r.deleteCredential))

	container.Add(ws)
}

// RegisterWeb registers extension web bundle on the container
func (r Resource) RegisterWeb(container *restful.Container) {
	var handler http.Handler
	webResourcesDir := os.Getenv("WEB_RESOURCES_DIR")
	koDataPath := os.Getenv("KO_DATA_PATH")
	_, err := os.Stat(webResourcesDir)
	if err != nil {
		if os.IsNotExist(err) {
			if koDataPath != "" {
				logging.Log.Warnf("WEB_RESOURCES_DIR %s not found, serving static content from KO_DATA_PATH instead.", webResourcesDir)
				handler = http.FileServer(http.Dir(koDataPath))
			} else {
				logging.Log.Errorf("WEB_RESOURCES_DIR %s not found and KO_DATA_PATH not found, static resource (UI) problems to be expected.", webResourcesDir)
			}
		} else {
			logging.Log.Errorf("error returned while checking for WEB_RESOURCES_DIR %s", webResourcesDir)
		}
	} else {
		logging.Log.Infof("Serving static files from WEB_RESOURCES_DIR: %s", webResourcesDir)
		handler = http.FileServer(http.Dir(webResourcesDir))
	}
	container.Handle("/web/", http.StripPrefix("/web/", handler))
}

// createOpenshiftRoute attempts to create an Openshift Route on the service.
// The Route has the same name as the service
func (r Resource) createOpenshiftRoute(serviceName string) error {
	annotations := make(map[string]string)
	annotations["haproxy.router.openshift.io/timeout"] = "2m"

	route := &routesv1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceName,
			Annotations: annotations,
		},
		Spec: routesv1.RouteSpec{
			To: routesv1.RouteTargetReference{
				Kind: "Service",
				Name: serviceName,
			},
			TLS: &routesv1.TLSConfig{
				Termination:                   "edge",
				InsecureEdgeTerminationPolicy: "Redirect",
			},
		},
	}
	_, err := r.RoutesClient.RouteV1().Routes(r.Defaults.Namespace).Create(route)
	return err
}

// deleteOpenshiftRoute attempts to delete an Openshift Route
func (r Resource) deleteOpenshiftRoute(routeName string) error {
	return r.RoutesClient.RouteV1().Routes(r.Defaults.Namespace).Delete(routeName, &metav1.DeleteOptions{})
}
