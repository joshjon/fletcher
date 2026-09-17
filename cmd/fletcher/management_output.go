package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
)

func renderManagement(format string, m proto.Message) error {
	if format == "json" {
		b, err := (protojson.MarshalOptions{Indent: "  "}).Marshal(m)
		if err == nil {
			fmt.Println(string(b))
		}
		return err
	}
	if format != "" && format != "table" {
		return fmt.Errorf("unknown output format %q", format)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	switch r := m.(type) {
	case *fletcherv1.GetHostResponse:
		fmt.Fprintf(w, "version\t%s\ncommit\t%s\nstarted\t%s\nrestart supported\t%t\n", r.GetVersion(), r.GetCommit(), time.Unix(r.GetStartedAt(), 0).Format(time.RFC3339), r.GetCanRestart())
		if r.GetRestartUnavailableReason() != "" {
			fmt.Fprintln(w, r.GetRestartUnavailableReason())
		}
		for _, c := range r.GetChecks() {
			fmt.Fprintf(w, "%s\t%s\t%s\n", c.GetStatus(), c.GetName(), c.GetDetail())
		}
	case *fletcherv1.GetStorageResponse:
		fmt.Fprintf(w, "capacity\t%.2f GiB\navailable\t%.2f GiB\n", float64(r.GetTotalBytes())/(1<<30), float64(r.GetAvailableBytes())/(1<<30))
		fmt.Fprintln(w, "CATEGORY\tALLOCATED\tLOGICAL\tFILES")
		for _, c := range r.GetCategories() {
			fmt.Fprintf(w, "%s\t%.2f GiB\t%.2f GiB\t%d\n", c.GetName(), float64(c.GetAllocatedBytes())/(1<<30), float64(c.GetLogicalBytes())/(1<<30), c.GetFiles())
		}
		fmt.Fprintln(w, "Shared CoW blocks may be counted more than once; category totals are not reclaimable space.")
	case *fletcherv1.ListImagesResponse:
		fmt.Fprintln(w, "NAME\tFORMAT\tSOURCE")
		for _, img := range r.GetImages() {
			fmt.Fprintf(w, "%s\t%s\t%s\n", img.GetName(), img.GetFormat(), img.GetSource())
		}
	case *fletcherv1.StartImportResponse:
		fmt.Fprintf(w, "import %s\t%s\t%s\n", r.GetOperation().GetId(), r.GetOperation().GetName(), r.GetOperation().GetState())
		if r.GetOperation().GetState() == "running" {
			fmt.Fprintln(w, "Import continues on the daemon. Check progress with 'fletcher image imports'.")
		}
	case *fletcherv1.ListImportsResponse:
		fmt.Fprintln(w, "ID\tNAME\tSTATE\tERROR")
		for _, op := range r.GetOperations() {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", op.GetId(), op.GetName(), op.GetState(), op.GetError())
		}
	case *fletcherv1.RunDiagnosticsResponse:
		for _, c := range r.GetChecks() {
			fmt.Fprintf(w, "%s\t%s\t%s\n", c.GetStatus(), c.GetName(), c.GetDetail())
		}
		fmt.Fprintln(w, r.GetActionPlan())
	default:
		return fmt.Errorf("unsupported management response")
	}
	return w.Flush()
}
