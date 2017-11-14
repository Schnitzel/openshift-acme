package route

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/rand"
	"time"

	"github.com/golang/glog"
	routev1 "github.com/openshift/api/route/v1"
	routeclientset "github.com/openshift/client-go/route/clientset/versioned"
	_ "github.com/openshift/client-go/route/clientset/versioned/scheme"
	routelistersv1 "github.com/openshift/client-go/route/listers/route/v1"
	"golang.org/x/crypto/acme"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	kcorelistersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"github.com/tnozicka/openshift-acme/pkg/acme/challengeexposers"
	acmeclient "github.com/tnozicka/openshift-acme/pkg/acme/client"
	acmeclientbuilder "github.com/tnozicka/openshift-acme/pkg/acme/client/builder"
	"github.com/tnozicka/openshift-acme/pkg/api"
	"github.com/tnozicka/openshift-acme/pkg/cert"
	routeutil "github.com/tnozicka/openshift-acme/pkg/route"
	"github.com/tnozicka/openshift-acme/pkg/util"
)

const (
	ControllerName           = "openshift-acme-controller"
	MaxRetries               = 1
	RenewalStandardDeviation = 1
	RenewalMean              = 0
	AcmeTimeout              = 10 * time.Second
)

var (
	KeyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc
)

type RouteController struct {
	acmeClientFactory *acmeclientbuilder.SharedClientFactory

	// TODO: switch this for generic interface to allow other types like DNS01
	exposers map[string]challengeexposers.Interface

	routeIndexer cache.Indexer

	routeClientset routeclientset.Interface
	kubeClientset  kubernetes.Interface

	routeInformer  cache.SharedIndexInformer
	secretInformer cache.SharedIndexInformer

	routeLister  routelistersv1.RouteLister
	secretLister kcorelistersv1.SecretLister

	// routeInformerSynced returns true if the Route store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	routeInformerSynced cache.InformerSynced

	// secretInformerSynced returns true if the Secret store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	secretInformerSynced cache.InformerSynced

	recorder record.EventRecorder

	queue workqueue.RateLimitingInterface

	//selfServiceNamespace, selfServiceName string
	exposerIP string
}

func NewRouteController(
	acmeClientFactory *acmeclientbuilder.SharedClientFactory,
	exposers map[string]challengeexposers.Interface,
	routeClientset routeclientset.Interface,
	kubeClientset kubernetes.Interface,
	routeInformer cache.SharedIndexInformer,
	secretInformer cache.SharedIndexInformer,
	exposerIP string,
	//selfServiceNamespace, selfServiceName string,
) *RouteController {

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClientset.CoreV1().Events("")})

	rc := &RouteController{
		acmeClientFactory: acmeClientFactory,

		exposers: exposers,

		routeIndexer: routeInformer.GetIndexer(),

		routeClientset: routeClientset,
		kubeClientset:  kubeClientset,

		routeInformer:  routeInformer,
		secretInformer: secretInformer,

		routeLister:  routelistersv1.NewRouteLister(routeInformer.GetIndexer()),
		secretLister: kcorelistersv1.NewSecretLister(secretInformer.GetIndexer()),

		routeInformerSynced:  routeInformer.HasSynced,
		secretInformerSynced: secretInformer.HasSynced,

		recorder: eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: ControllerName}),

		queue: workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),

		//selfServiceNamespace: selfServiceNamespace,
		//selfServiceName:      selfServiceName,
		exposerIP: exposerIP,
	}

	routeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    rc.addRoute,
		UpdateFunc: rc.updateRoute,
		DeleteFunc: rc.deleteRoute,
	})
	//secretInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
	//	AddFunc:    rc.addSecret,
	//	UpdateFunc: rc.updateSecret,
	//	DeleteFunc: rc.deleteSecret,
	//})

	return rc
}

func (rc *RouteController) enqueueRoute(route *routev1.Route) {
	key, err := KeyFunc(route)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %#v: %v", route, err))
		return
	}

	rc.queue.Add(key)
}

func (rc *RouteController) addRoute(obj interface{}) {
	route := obj.(*routev1.Route)
	if !util.IsManaged(route) {
		glog.V(5).Infof("Skipping Route %s/%s UID=%s RV=%s", route.Namespace, route.Name, route.UID, route.ResourceVersion)
		return
	}

	glog.V(4).Infof("Adding Route %s/%s UID=%s RV=%s", route.Namespace, route.Name, route.UID, route.ResourceVersion)
	rc.enqueueRoute(route)
}

func (rc *RouteController) updateRoute(old, cur interface{}) {
	oldRoute := old.(*routev1.Route)
	newRoute := cur.(*routev1.Route)

	// A periodic relist will send update events for all known configs.
	if newRoute.ResourceVersion == oldRoute.ResourceVersion {
		return
	}

	if !util.IsManaged(newRoute) {
		glog.V(5).Infof("Skipping Route %s/%s UID=%s RV=%s", newRoute.Namespace, newRoute.Name, newRoute.UID, newRoute.ResourceVersion)
		return
	}

	glog.V(4).Infof("Updating Route from %s/%s UID=%s RV=%s to %s/%s UID=%s,RV=%s",
		oldRoute.Namespace, oldRoute.Name, oldRoute.UID, oldRoute.ResourceVersion,
		newRoute.Namespace, newRoute.Name, newRoute.UID, newRoute.ResourceVersion)

	rc.enqueueRoute(newRoute)
}

func (rc *RouteController) deleteRoute(obj interface{}) {
	route, ok := obj.(*routev1.Route)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("object is not a Route neither tombstone: %#v", obj))
			return
		}
		route, ok = tombstone.Obj.(*routev1.Route)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Route %#v", obj))
			return
		}
	}

	if !util.IsManaged(route) {
		glog.V(5).Infof("Skipping Route %s/%s UID=%s RV=%s", route.Namespace, route.Name, route.UID, route.ResourceVersion)
		return
	}

	glog.V(4).Infof("Deleting Route %s/%s UID=%s RV=%s", route.Namespace, route.Name, route.UID, route.ResourceVersion)
	rc.enqueueRoute(route)
}

// TODO: extract this function to be re-used by ingress controller
// FIXME: needs expectation protection
func (rc *RouteController) getState(t time.Time, route *routev1.Route) api.AcmeState {
	if route.Annotations != nil {
		_, ok := route.Annotations[api.AcmeAwaitingAuthzUrlAnnotation]
		if ok {
			return api.AcmeStateWaitingForAuthz
		}
	}

	if route.Spec.TLS == nil {
		return api.AcmeStateNeedsCert
	}

	certPemData := &cert.CertPemData{
		Key: []byte(route.Spec.TLS.Key),
		Crt: []byte(route.Spec.TLS.Certificate),
	}
	certificate, err := certPemData.Certificate()
	if err != nil {
		glog.Errorf("Failed to decode certificate from route %s/%s", route.Namespace, route.Name)
		return api.AcmeStateNeedsCert
	}

	err = certificate.VerifyHostname(route.Spec.Host)
	if err != nil {
		glog.Errorf("Certificate is invalid for route %s/%s with hostname %q", route.Namespace, route.Name, route.Spec.Host)
		return api.AcmeStateNeedsCert
	}

	if !cert.IsValid(certificate, t) {
		return api.AcmeStateNeedsCert
	}

	// We need to trigger renewals before the certs expire
	remains := certificate.NotAfter.Sub(t)
	lifetime := certificate.NotAfter.Sub(certificate.NotBefore)

	// This is the deadline when we start renewing
	if remains <= lifetime/3 {
		glog.Infof("Renewing cert because we reached a deadline of %s", remains)
		return api.AcmeStateNeedsCert
	}

	// In case many certificates were provisioned at specific time
	// We will try to avoid spikes by renewing randomly
	if remains <= lifetime/2 {
		// We need to randomize renewals to spread the load.
		// Closer to deadline, bigger chance
		s := rand.NewSource(t.UnixNano())
		r := rand.New(s)
		n := r.NormFloat64()*RenewalStandardDeviation + RenewalMean
		// We use left half of normal distribution (all negative numbers).
		if n < 0 {
			glog.V(4).Infof("Renewing cert in advance with %s remaining to spread the load.", remains)
			return api.AcmeStateNeedsCert
		}
	}

	return api.AcmeStateOk
}

func (rc *RouteController) wrapExposers(exposers map[string]challengeexposers.Interface, route *routev1.Route) map[string]challengeexposers.Interface {
	wrapped := make(map[string]challengeexposers.Interface)

	for k, v := range exposers {
		if k == "http-01" {
			//wrapped[k] = NewExposer(v, rc.routeClientset, rc.kubeClientset, route, rc.selfServiceName, rc.selfServiceNamespace)
			wrapped[k] = NewExposer(v, rc.routeClientset, rc.kubeClientset, route, rc.exposerIP)
		} else {
			wrapped[k] = v
		}
	}

	return wrapped
}

// handle is the business logic of the controller.
// In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
// This function is not meant to be invoked concurrently with the same key.
// TODO: extract common parts to be re-used by ingress controller
func (rc *RouteController) handle(key string) error {
	startTime := time.Now()
	glog.V(4).Infof("Started syncing Route %q (%v)", key, startTime)
	defer func() {
		glog.V(4).Infof("Finished syncing Route %q (%v)", key, time.Since(startTime))
	}()

	objReadOnly, exists, err := rc.routeIndexer.GetByKey(key)
	if err != nil {
		glog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		glog.V(4).Infof("Route %s does not exist anymore\n", key)
		return nil
	}

	// Deep copy to avoid mutating the cache
	routeReadOnly := objReadOnly.(*routev1.Route)

	// Don't act on objects that are being deleted
	if routeReadOnly.DeletionTimestamp != nil {
		return nil
	}

	// We have to check if Route is admitted to be sure it owns the domain!
	if !routeutil.IsAdmitted(routeReadOnly) {
		glog.V(4).Infof("Skipping Route %s because it's not admitted", key)
		return nil
	}

	if routeReadOnly.Annotations[api.TlsAcmePausedAnnotation] == "true" {
		glog.V(4).Infof("Skipping Route %s because it is paused", key)

		// TODO: reconcile (e.g. related secrets)
		return nil
	}

	state := rc.getState(startTime, routeReadOnly)
	// FIXME: this state machine needs protection with expectations
	// (informers may not be synced yet with recent state transition updates)
	switch state {
	case api.AcmeStateNeedsCert:
		// TODO: Add TTL based lock to allow only one domain to enter this stage

		ctx, cancel := context.WithTimeout(context.Background(), AcmeTimeout)
		defer cancel()

		client, err := rc.acmeClientFactory.GetClient(ctx)
		if err != nil {
			return err
		}

		// FIXME: definitely protect with expectations
		authorization, err := client.Client.Authorize(ctx, routeReadOnly.Spec.Host)
		if err != nil {
			return err
		}
		glog.V(4).Infof("Created authorization %q for Route %s", authorization.URI, key)

		if authorization.Status == acme.StatusValid {
			glog.V(4).Infof("Authorization %q for Route %s is already valid", authorization.URI, key)
		}

		route := routeReadOnly.DeepCopy()
		if route.Annotations == nil {
			route.Annotations = make(map[string]string)
		}
		route.Annotations[api.AcmeAwaitingAuthzUrlAnnotation] = authorization.URI
		_, err = rc.routeClientset.RouteV1().Routes(route.Namespace).Update(route)
		if err != nil {
			glog.Errorf("Failed to update Route %s: %v. Revoking authorization %q so it won't stay pending.", key, err, authorization.URI)
			// We need to try to cancel the authorization so we don't leave pending authorization behind and get rate limited
			acmeErr := client.Client.RevokeAuthorization(ctx, authorization.URI)
			if acmeErr != nil {
				glog.Errorf("Failed to revoke authorization %q: %v", acmeErr)
			}

			return err
		}

		return nil

	case api.AcmeStateWaitingForAuthz:
		ctx, cancel := context.WithTimeout(context.Background(), AcmeTimeout)
		defer cancel()

		client, err := rc.acmeClientFactory.GetClient(ctx)
		if err != nil {
			return err
		}

		authorizationUri := routeReadOnly.Annotations[api.AcmeAwaitingAuthzUrlAnnotation]
		authorization, err := client.Client.GetAuthorization(ctx, authorizationUri)
		// TODO: emit an event but don't fail as user can set it
		if err != nil {
			return err
		}

		glog.V(4).Infof("Route %q: authorization state is %q", key, authorization.Status)

		switch authorization.Status {
		case acme.StatusPending:
			exposers := rc.wrapExposers(rc.exposers, routeReadOnly)
			authorization, err := client.AcceptAuthorization(ctx, authorization, routeReadOnly.Spec.Host, exposers)
			if err != nil {
				return err
			}

			if authorization.Status == acme.StatusPending {
				glog.V(4).Infof("Re-queuing Route %q due to pending authorization", key)

				// TODO: get this value from authorization when this is fixed
				// https://github.com/golang/go/issues/22457
				retryAfter := 5 * time.Second
				rc.queue.AddAfter(key, retryAfter)

				// Don't count this as requeue, reset counter
				rc.queue.Forget(key)

				return nil
			}

			if authorization.Status != acme.StatusValid {
				return fmt.Errorf("route %q - authorization has transitioned to unexpected state %q", key, authorization.Status)
			}

			fallthrough

		case acme.StatusValid:
			glog.V(4).Infof("Authorization %q for Route %s successfully validated", authorization.URI, key)
			// provision cert
			template := x509.CertificateRequest{
				Subject: pkix.Name{
					CommonName: routeReadOnly.Spec.Host,
				},
			}
			template.DNSNames = append(template.DNSNames, routeReadOnly.Spec.Host)
			privateKey, err := rsa.GenerateKey(cryptorand.Reader, 4096)
			if err != nil {
				return err
			}

			csr, err := x509.CreateCertificateRequest(cryptorand.Reader, &template, privateKey)
			if err != nil {
				return err
			}

			// TODO: protect with expectations
			// TODO: aks to split CreateCert func in acme library to avoid embedded pooling
			der, certUrl, err := client.Client.CreateCert(ctx, csr, 0, true)
			if err != nil {
				return err
			}
			glog.V(4).Infof("Route %q - created certificate available at %s", key, certUrl)

			certPemData, err := cert.NewCertificateFromDER(der, privateKey)
			if err != nil {
				return err
			}

			route := routeReadOnly.DeepCopy()
			if route.Spec.TLS == nil {
				route.Spec.TLS = &routev1.TLSConfig{
					// Defaults
					InsecureEdgeTerminationPolicy: "Redirect",
					Termination:                   routev1.TLSTerminationEdge,
				}
			}
			route.Spec.TLS.Key = string(certPemData.Key)
			route.Spec.TLS.Certificate = string(certPemData.Crt)

			delete(route.Annotations, api.AcmeAwaitingAuthzUrlAnnotation)

			route, err = rc.routeClientset.RouteV1().Routes(route.Namespace).Update(route)
			if err != nil {
				return err
			}

			rc.recorder.Event(route, corev1.EventTypeNormal, "AcmeCertificateProvisioned", "Successfully provided new certificate")

		case acme.StatusInvalid:
			rc.recorder.Eventf(routeReadOnly, corev1.EventTypeWarning, "AcmeFailedAuthorization", "Acme provider failed to validate domain %q: %s", routeReadOnly.Spec.Host, acmeclient.GetAuthorizationErrors(authorization))

			route := routeReadOnly.DeepCopy()
			delete(route.Annotations, api.AcmeAwaitingAuthzUrlAnnotation)
			// TODO: remove force pausing when we have ACME rate limiter
			route.Annotations[api.TlsAcmePausedAnnotation] = "true"
			route, err = rc.routeClientset.RouteV1().Routes(route.Namespace).Update(route)
			if err != nil {
				return err
			}

		case acme.StatusRevoked:
			rc.recorder.Eventf(routeReadOnly, corev1.EventTypeWarning, "AcmeRevokedAuthorization", "Acme authorization has been revoked for domain %q: %s", routeReadOnly.Spec.Host, acmeclient.GetAuthorizationErrors(authorization))

		case acme.StatusProcessing:
			fallthrough
		default:
			return fmt.Errorf("unknow authorization status %s", authorization.Status)
		}

	case api.AcmeStateOk:
	default:
		return fmt.Errorf("failed to determine state for Route: %#v", routeReadOnly)
	}

	// TODO: reconcile (e.g. related secrets)

	return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (rc *RouteController) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		rc.queue.Forget(key)
		return
	}

	if rc.queue.NumRequeues(key) < MaxRetries {
		glog.Infof("Error syncing Route %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		rc.queue.AddRateLimited(key)
		return
	}

	rc.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	glog.Infof("Dropping Route %q out of the queue: %v", key, err)
}

func (rc *RouteController) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := rc.queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two Routes with the same key are never processed in
	// parallel.
	defer rc.queue.Done(key)

	// Invoke the method containing the business logic
	err := rc.handle(key.(string))
	// Handle the error if something went wrong during the execution of the business logic
	rc.handleErr(err, key)
	return true
}

func (rc *RouteController) runWorker() {
	for rc.processNextItem() {
	}
}

func (rc *RouteController) Run(workers int, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()

	// Let the workers stop when we are done
	defer rc.queue.ShutDown()

	glog.Info("Starting Route controller")

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(stopCh, rc.routeInformerSynced, rc.secretInformerSynced) {
		runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(rc.runWorker, time.Second, stopCh)
	}

	<-stopCh

	glog.Info("Stopping Route controller")
}