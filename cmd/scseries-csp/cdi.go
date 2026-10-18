package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var storageProfileGVR = schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "storageprofiles"}

// cdiProfiles sets RWX raw block (live-migratable) as the default claim properties of the CDI StorageProfiles
// of the given StorageClasses. CDI creates StorageProfiles itself, so they are patched, not owned.
func cdiProfiles(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("cdi-profiles", flag.ExitOnError)
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig (default: in-cluster)")
	classes := fs.String("classes", "", "comma-separated StorageClass names")
	wait := fs.Duration("wait", 5*time.Minute, "how long to wait for CDI to create the StorageProfiles")
	_ = fs.Parse(args)
	if *classes == "" {
		return fmt.Errorf("--classes is required")
	}
	var rc *rest.Config
	var err error
	if *kubeconfig != "" {
		rc, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		rc, err = rest.InClusterConfig()
	}
	if err != nil {
		return err
	}
	dc, err := dynamic.NewForConfig(rc)
	if err != nil {
		return err
	}
	patch := []byte(`{"spec":{"claimPropertySets":[{"accessModes":["ReadWriteMany"],"volumeMode":"Block"}],"cloneStrategy":"csi-clone"}}`)
	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()
	for _, name := range strings.Split(*classes, ",") {
		name = strings.TrimSpace(name)
		for {
			_, err := dc.Resource(storageProfileGVR).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
			if err == nil {
				log.Info("storage profile set to RWX raw block", "storageClass", name)
				break
			}
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("patch storage profile %s: %w", name, err)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("storage profile %s not created by CDI within %s", name, *wait)
			case <-time.After(3 * time.Second):
			}
		}
	}
	return nil
}
