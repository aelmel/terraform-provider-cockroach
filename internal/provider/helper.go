package provider

import (
	"context"
	"fmt"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
)

func tryPortForwardIfNeeded(ctx context.Context, d *schema.ResourceData, meta interface{}, stopCh chan struct{}, readyCh chan struct{}, localPort string) diag.Diagnostics {
	cockroachClient := meta.(*cockroachClient)

	if kubeConfig := cockroachClient.kubeConn.kubeConfig; kubeConfig != nil {
		kubeClientSet := cockroachClient.kubeConn.kubeClient
		nameSpace := cockroachClient.kubeConn.nameSpace
		serviceName := cockroachClient.kubeConn.serviceName
		remotePort := cockroachClient.kubeConn.remotePort

		errCh := make(chan error, 1)

		// managing termination signal from the terminal. As you can see the stopCh
		// gets closed to gracefully handle its termination.
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigs
			logInfo("Stopping a forward process...")
			close(stopCh)
		}()

		go func() {
			defer close(errCh)
			
			svc, err := kubeClientSet.CoreV1().Services(nameSpace).Get(ctx, serviceName, metav1.GetOptions{})
			if err != nil {
				logError("failed to get Kubernetes service %s in namespace %s: %v", serviceName, nameSpace, err)
				errCh <- fmt.Errorf("failed to get Kubernetes service: %w", err)
				return
			}

			selector := mapToSelectorStr(svc.Spec.Selector)
			if selector == "" {
				err := fmt.Errorf("service %s has no selector", serviceName)
				logError("failed to get service selector: %v", err)
				errCh <- err
				return
			}

			pods, err := kubeClientSet.CoreV1().Pods(svc.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				logError("failed to get pod list for selector %s: %v", selector, err)
				errCh <- fmt.Errorf("failed to get pod list: %w", err)
				return
			}

			if len(pods.Items) == 0 {
				err := fmt.Errorf("no CockroachDB pods found with selector %s", selector)
				logError("%v", err)
				errCh <- err
				return
			}

			livePod, err := getPodName(pods)
			if err != nil {
				logError("failed to get live CockroachDB pod: %v", err)
				errCh <- fmt.Errorf("failed to get live pod: %w", err)
				return
			}

			serverURL, err := url.Parse(
				fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s/portforward", kubeConfig.Host, nameSpace, livePod))
			if err != nil {
				logError("failed to construct server URL: %v", err)
				errCh <- fmt.Errorf("failed to construct server URL: %w", err)
				return
			}

			transport, upgrader, err := spdy.RoundTripperFor(kubeConfig)
			if err != nil {
				logError("failed to create round tripper: %v", err)
				errCh <- fmt.Errorf("failed to create round tripper: %w", err)
				return
			}

			dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, serverURL)

			addresses := []string{"127.0.0.1"}
			ports := []string{fmt.Sprintf("%s:%s", localPort, remotePort)}

			pf, err := portforward.NewOnAddresses(
				dialer,
				addresses,
				ports,
				stopCh,
				readyCh,
				os.Stdout,
				os.Stderr)
			if err != nil {
				logError("failed to create port-forward %s:%s: %v", localPort, remotePort, err)
				errCh <- fmt.Errorf("failed to create port-forward: %w", err)
				return
			}

			go pf.ForwardPorts()

			<-readyCh

			actualPorts, err := pf.GetPorts()
			if err != nil {
				logError("failed to get port-forward ports: %v", err)
				errCh <- fmt.Errorf("failed to get port-forward ports: %w", err)
				return
			}
			if len(actualPorts) != 1 {
				err := fmt.Errorf("unexpected number of forwarded ports: got %d, expected 1", len(actualPorts))
				logError("%v", err)
				errCh <- err
				return
			}
			
			logInfo("Port forwarding established: %s:%s -> %s", localPort, remotePort, livePod)
		}()

		select {
		case <-readyCh:
			logDebug("Port-forwarding is ready to handle traffic")
			break
		case err := <-errCh:
			return diag.FromErr(err)
		}
	}

	return nil
}

func getPodName(pods *v1.PodList) (string, error) {

	for _, pod := range pods.Items {
		if pod.Status.Phase != v1.PodRunning {
			continue
		}

		return pod.Name, nil
	}

	return "", fmt.Errorf("no live pods behind the service")
}

func mapToSelectorStr(msel map[string]string) string {
	selector := ""
	for k, v := range msel {
		if selector != "" {
			selector = selector + ","
		}
		selector = selector + fmt.Sprintf("%s=%s", k, v)
	}

	return selector
}

func convertToString(raw_list []interface{}) []string {
	val := make([]string, len(raw_list))
	for i, raw := range raw_list {
		val[i] = raw.(string)
	}

	return val
}

func contains(elems []string, v string) bool {
	for _, s := range elems {
		if v == s {
			return true
		}
	}
	return false
}
