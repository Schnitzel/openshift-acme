package framework

import (
	"fmt"
	"sort"
	"time"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"

	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/kubernetes/pkg/client/retry"

	projectapi "github.com/openshift/origin/pkg/project/apis/project"
)

func CreateTestingNamespace(f *Framework, name string, labels map[string]string) (*v1.Namespace, error) {
	if labels == nil {
		labels = map[string]string{}
	}

	namespaceObj := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("e2e-tests-%v-", name),
			Labels:       labels,
		},
	}

	// Be robust about making the namespace creation call.
	var got *v1.Namespace
	if err := wait.PollImmediate(2*time.Second, 30*time.Second, func() (bool, error) {
		var err error
		got, err = f.KubeClientSet().CoreV1().Namespaces().Create(namespaceObj)
		if err != nil {
			Logf("Unexpected error while creating namespace: %v", err)
			return false, nil
		}
		return true, nil
	}); err != nil {
		return nil, err
	}

	w, err := f.KubeClientSet().CoreV1().ServiceAccounts(got.Name).Watch(metav1.SingleObject(metav1.ObjectMeta{Name: "default"}))
	if err != nil {
		return got, err
	}
	_, err = watch.Until(30*time.Second, w, func(event watch.Event) (bool, error) {
		switch t := event.Object.(type) {
		case *v1.ServiceAccount:
			return len(t.Secrets) > 0, nil
		}
		return false, nil
	})

	return got, nil
}

func CreateProject(f *Framework, name string, labels map[string]string) (*v1.Namespace, error) {
	Logf("************** %#v", labels)
	_, err := f.LegacyClient().ProjectRequests().Create(&projectapi.ProjectRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	})
	if err != nil {
		return nil, err
	}

	err = wait.ExponentialBackoff(retry.DefaultBackoff, func() (bool, error) {
		_, err := f.KubeClientSet().CoreV1().Pods(name).List(metav1.ListOptions{})
		if err != nil {
			if apierrs.IsForbidden(err) {
				Logf("Waiting for user to have access to the namespace")
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	return &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, nil
}

func CreateTestingProjectAndChangeUser(f *Framework, name string, labels map[string]string) (*v1.Namespace, error) {
	name = names.SimpleNameGenerator.GenerateName(fmt.Sprintf("e2e-test-%s-", name))

	f.ChangeUser(fmt.Sprintf("%s-user", name), name)
	Logf("The user is now %q", f.Username())

	Logf("Creating project %q", name)
	namespace, err := CreateProject(f, name, labels)
	if err != nil {
		return nil, err
	}

	return namespace, nil
}

func DumpEventsInNamespace(c kubernetes.Interface, namespace string) {
	events, err := c.CoreV1().Events(namespace).List(metav1.ListOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())

	g.By(fmt.Sprintf("Found %d events.", len(events.Items)))
	// Sort events by their first timestamp
	sortedEvents := events.Items
	if len(sortedEvents) > 1 {
		sort.Sort(byFirstTimestamp(sortedEvents))
	}
	for _, e := range sortedEvents {
		Logf("At %v - event for %v: %v %v: %v", e.FirstTimestamp, e.InvolvedObject.Name, e.Source, e.Reason, e.Message)
	}
}

// byFirstTimestamp sorts a slice of events by first timestamp, using their involvedObject's name as a tie breaker.
type byFirstTimestamp []v1.Event

func (o byFirstTimestamp) Len() int {
	return len(o)
}

func (o byFirstTimestamp) Swap(i, j int) {
	o[i], o[j] = o[j], o[i]
}

func (o byFirstTimestamp) Less(i, j int) bool {
	if o[i].FirstTimestamp.Equal(o[j].FirstTimestamp) {
		return o[i].InvolvedObject.Name < o[j].InvolvedObject.Name
	}
	return o[i].FirstTimestamp.Before(o[j].FirstTimestamp)
}
