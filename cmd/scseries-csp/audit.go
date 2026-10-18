package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kube-dc/scseries-csp/internal/csp"
	"github.com/kube-dc/scseries-csp/internal/sc"
)

func metaFor(name, ns string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: ns} }

const driverName = "csi.hpe.com"

// audit compares the CSP-owned objects on the array with the cluster and prints findings.
func audit(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	kubeconfig := fs.String("kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig (default: in-cluster)")
	fixList := fs.String("fix", "", "comma-separated finding kinds to repair: stale-mapping,stale-server (orphan and missing volumes are only reported)")
	fixMatch := fs.String("fix-match", ".*", "only repair findings whose object (volume or server name) matches this regex")
	_ = fs.Parse(args)
	matchRe, err := regexp.Compile(*fixMatch)
	if err != nil {
		return fmt.Errorf("--fix-match: %w", err)
	}
	fix := map[string]bool{}
	for _, k := range strings.Split(*fixList, ",") {
		switch k = strings.TrimSpace(k); k {
		case "":
		case "stale-mapping", "stale-server":
			fix[k] = true
		default:
			return fmt.Errorf("--fix: %q cannot be repaired automatically (allowed: stale-mapping, stale-server)", k)
		}
	}

	host, user, pw := os.Getenv("SC_HOST"), env("CSP_SC_USER", os.Getenv("SC_USER")), env("CSP_SC_PASSWORD", os.Getenv("SC_PASSWORD"))
	if host == "" || user == "" || pw == "" {
		return fmt.Errorf("set SC_HOST and SC_USER/SC_PASSWORD (or CSP_SC_USER/CSP_SC_PASSWORD)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var rc *rest.Config
	if *kubeconfig != "" {
		rc, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		rc, err = rest.InClusterConfig()
	}
	if err != nil {
		return err
	}
	kc, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	dc, err := dynamic.NewForConfig(rc)
	if err != nil {
		return err
	}
	ac := arrayConfig(host, user, pw)
	if ac.Fingerprint == "" && ac.CAFile == "" {
		ac.InsecureSkipVerify = envBool("SC_TLS_INSECURE", true)
	}
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c, err := sc.New(ctx, ac, quiet, nil)
	if err != nil {
		return err
	}
	defer c.Logout(ctx)
	cfg := serviceConfig()

	// cluster side
	pvs, err := kc.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	pvByHandle := map[string]string{}
	for _, pv := range pvs.Items {
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == driverName {
			pvByHandle[pv.Spec.CSI.VolumeHandle] = pv.Name
		}
	}
	vas, err := kc.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	attached := map[string]bool{} // pvName/node
	for _, va := range vas.Items {
		if va.Spec.Attacher == driverName && va.Spec.Source.PersistentVolumeName != nil {
			attached[*va.Spec.Source.PersistentVolumeName+"/"+va.Spec.NodeName] = true
		}
	}
	nodeByUUID := map[string]string{}
	infos, err := dc.Resource(schema.GroupVersionResource{Group: "storage.hpe.com", Version: "v1", Resource: "hpenodeinfos"}).
		List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, it := range infos.Items {
			if uuid, _, _ := unstructuredString(it.Object, "spec", "uuid"); uuid != "" {
				nodeByUUID[uuid] = it.GetName()
			}
		}
	}
	nodes, err := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	nodeExists := map[string]bool{}
	for _, n := range nodes.Items {
		nodeExists[n.Name] = true
	}

	// array side
	var vols []sc.Volume
	if err := c.GetList(ctx, "ScVolume", nil, &vols); err != nil {
		return err
	}
	var servers []sc.Server
	if err := c.GetList(ctx, "ScPhysicalServer", nil, &servers); err != nil {
		return err
	}
	serverNode := map[string]string{} // server id -> node name ("" if unknown)
	type finding struct{ kind, object, detail, repair string }
	var out []finding
	for _, s := range servers {
		if !strings.HasPrefix(strings.Trim(s.ServerFolderPath, "/")+"/", cfg.ServerFolder+"/") {
			continue
		}
		uuid := csp.NodeUUIDFromNotes(s.Notes)
		node := nodeByUUID[uuid]
		serverNode[s.InstanceID] = node
		if node == "" || !nodeExists[node] {
			f := finding{kind: "stale-server", object: s.Name, detail: fmt.Sprintf("SC server %s (uuid %q) has no matching cluster node", s.InstanceID, uuid)}
			if fix["stale-server"] && matchRe.MatchString(s.Name) {
				switch {
				case s.VolumeCount > 0:
					f.repair = "skipped: volumes still mapped"
				default:
					if err := c.Delete(ctx, "StorageCenter/ScServer/"+s.InstanceID); err != nil {
						f.repair = "FAILED: " + err.Error()
					} else {
						f.repair = "deleted server object"
					}
				}
			}
			out = append(out, f)
		}
	}
	owned := map[string]bool{}
	for _, v := range vols {
		if v.InRecycleBin || !strings.HasPrefix(strings.Trim(v.VolumeFolderPath, "/")+"/", cfg.VolumeFolder+"/") {
			continue
		}
		owned[v.InstanceID] = true
		pv := pvByHandle[v.InstanceID]
		if pv == "" {
			out = append(out, finding{kind: "orphan-volume", object: v.Name, detail: fmt.Sprintf("SC volume %s (%d GiB) has no PV", v.InstanceID, sc.Bytes(v.ConfiguredSize)>>30)})
		}
		if !v.Mapped {
			continue
		}
		var profiles []sc.MappingProfile
		if err := c.Get(ctx, "StorageCenter/ScVolume/"+v.InstanceID+"/MappingProfileList", &profiles); err != nil {
			return err
		}
		for _, mp := range profiles {
			node := serverNode[mp.Server.ID()]
			if pv == "" || node == "" || !attached[pv+"/"+node] {
				f := finding{kind: "stale-mapping", object: v.Name, detail: fmt.Sprintf("mapped to %s (node %q) without a VolumeAttachment", mp.Server.InstanceName, node)}
				if fix["stale-mapping"] && matchRe.MatchString(v.Name) {
					if err := c.Delete(ctx, "StorageCenter/ScMappingProfile/"+mp.InstanceID); err != nil {
						f.repair = "FAILED: " + err.Error()
					} else {
						f.repair = "unmapped"
					}
				}
				out = append(out, f)
			}
		}
	}
	for handle, pv := range pvByHandle {
		if !owned[handle] {
			out = append(out, finding{kind: "missing-volume", object: pv, detail: fmt.Sprintf("PV references SC volume %s which is not in folder %q", handle, cfg.VolumeFolder)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].kind+out[i].object < out[j].kind+out[j].object })
	fmt.Printf("audit of SC %s folders %q/%q: %d volumes, %d PVs, %d attachments, %d findings\n",
		c.SSN(), cfg.VolumeFolder, cfg.ServerFolder, len(owned), len(pvByHandle), len(attached), len(out))
	if len(out) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tOBJECT\tDETAIL\tREPAIR")
	for _, f := range out {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", f.kind, f.object, f.detail, f.repair)
	}
	_ = tw.Flush()
	for _, f := range out {
		if !strings.HasPrefix(f.repair, "unmapped") && !strings.HasPrefix(f.repair, "deleted") {
			os.Exit(2) // unresolved findings remain
		}
	}
	return nil
}

func unstructuredString(obj map[string]any, fields ...string) (string, bool, error) {
	cur := any(obj)
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false, nil
		}
		cur = m[f]
	}
	s, ok := cur.(string)
	return s, ok, nil
}
