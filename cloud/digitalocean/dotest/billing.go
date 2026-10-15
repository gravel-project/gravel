package dotest

import (
	"net/http"

	"github.com/digitalocean/godo"
)

// getDefaultProject and getProject answer one project, as the API does under "project".
func (s *Server) getDefaultProject(w http.ResponseWriter, _ *http.Request) {
	for _, p := range s.projects {
		if p.IsDefault {
			writeJSON(w, http.StatusOK, map[string]any{"project": p})
			return
		}
	}
	notFound(w)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	for _, p := range s.projects {
		if p.ID == r.PathValue("id") {
			writeJSON(w, http.StatusOK, map[string]any{"project": p})
			return
		}
	}
	notFound(w)
}

// listInvoices answers the team's invoices: none finished here, and the preview's summary.
func (s *Server) listInvoices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"invoices":        []godo.InvoiceListItem{},
		"invoice_preview": godo.InvoiceListItem{InvoiceUUID: "preview", Amount: "0.00", InvoicePeriod: s.invoiceUpdated.Format("2006-01"), UpdatedAt: s.invoiceUpdated},
		"links":           godo.Links{Pages: &godo.Pages{}},
		"meta":            godo.Meta{Total: 0},
	})
}

// invoicePreview answers a page of the preview's items, every project's, as the API does.
func (s *Server) invoicePreview(w http.ResponseWriter, r *http.Request) {
	items := s.invoiceItems
	if items == nil {
		items = []godo.InvoiceItem{}
	}
	s.writeList(w, r, "invoice_items", items)
}
