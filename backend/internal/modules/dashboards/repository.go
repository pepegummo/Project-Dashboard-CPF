package dashboards

import (
	"context"
	"encoding/json"
	"iot-dashboard/internal/database"
	"time"
)

type DashCount struct {
	Widgets int `json:"widgets"`
}

type Dashboard struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Description    *string         `json:"description"`
	OrganizationID string          `json:"organizationId"`
	UserID         string          `json:"userId"`
	IsPublic       bool            `json:"isPublic"`
	IsDefault      bool            `json:"isDefault"`
	Tags           []string        `json:"tags"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	Count          *DashCount      `json:"_count,omitempty"`
	WidgetLayouts  json.RawMessage `json:"widgetLayouts"`
	Widgets        []Widget        `json:"widgets,omitempty"`
}

type WidgetMachineField struct {
	Key        string   `json:"key"`
	Label      string   `json:"label"`
	Unit       *string  `json:"unit"`
	Threshold  *float64 `json:"threshold"`
	UpperLimit *float64 `json:"upperLimit"`
	LowerLimit *float64 `json:"lowerLimit"`
	Precision  int      `json:"precision"`
	IsKey      bool     `json:"isKey"`
}

type WidgetMachine struct {
	ID     string               `json:"id"`
	Name   string               `json:"name"`
	Type   string               `json:"type"`
	Fields []WidgetMachineField `json:"fields"`
}

type Widget struct {
	ID          string          `json:"id"`
	DashboardID string          `json:"dashboardId"`
	MachineID   *string         `json:"machineId"`
	WidgetType  string          `json:"widgetType"`
	Title       *string         `json:"title"`
	Layout      json.RawMessage `json:"layout"`
	Config      json.RawMessage `json:"config"`
	CreatedAt   time.Time       `json:"createdAt"`
	Machine     *WidgetMachine  `json:"machine,omitempty"`
}

type Repository struct{}

func (r *Repository) FindAll(ctx context.Context, orgID, userID string) ([]Dashboard, error) {
	rows, err := database.Pool.Query(ctx, `
		SELECT d.id, d.name, d.description, d.organization_id, d.user_id,
		       d.is_public, d.is_default, d.tags, d.created_at, d.updated_at,
		       (SELECT COUNT(*) FROM dashboard_widgets WHERE dashboard_id = d.id) AS widget_count,
		       COALESCE((SELECT JSON_AGG(JSON_BUILD_OBJECT('type', widget_type, 'x', (layout->>'x')::int, 'y', (layout->>'y')::int, 'w', (layout->>'w')::int, 'h', (layout->>'h')::int) ORDER BY created_at) FROM dashboard_widgets WHERE dashboard_id = d.id), '[]'::json) AS widget_layouts
		FROM dashboards d
		WHERE d.organization_id = $1
		ORDER BY d.created_at DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	dashboards := []Dashboard{} // ponytail: non-nil so empty org marshals to [] not null
	for rows.Next() {
		var d Dashboard
		var tags []string
		var widgetCount int
		var widgetLayouts json.RawMessage
		if err := rows.Scan(
			&d.ID, &d.Name, &d.Description, &d.OrganizationID, &d.UserID,
			&d.IsPublic, &d.IsDefault, &tags, &d.CreatedAt, &d.UpdatedAt,
			&widgetCount, &widgetLayouts,
		); err != nil {
			return nil, err
		}
		if tags != nil {
			d.Tags = tags
		} else {
			d.Tags = []string{}
		}
		if widgetLayouts == nil {
			widgetLayouts = json.RawMessage("[]")
		}
		d.Count = &DashCount{Widgets: widgetCount}
		d.WidgetLayouts = widgetLayouts
		dashboards = append(dashboards, d)
	}
	return dashboards, nil
}

func (r *Repository) FindByID(ctx context.Context, id string) (*Dashboard, error) {
	row := database.Pool.QueryRow(ctx, `
		SELECT d.id, d.name, d.description, d.organization_id, d.user_id,
		       d.is_public, d.is_default, d.tags, d.created_at, d.updated_at,
		       (SELECT COUNT(*) FROM dashboard_widgets WHERE dashboard_id = d.id) AS widget_count
		FROM dashboards d WHERE d.id = $1
	`, id)

	var d Dashboard
	var tags []string
	var widgetCount int
	if err := row.Scan(
		&d.ID, &d.Name, &d.Description, &d.OrganizationID, &d.UserID,
		&d.IsPublic, &d.IsDefault, &tags, &d.CreatedAt, &d.UpdatedAt,
		&widgetCount,
	); err != nil {
		return nil, err
	}
	if tags != nil {
		d.Tags = tags
	} else {
		d.Tags = []string{}
	}
	d.Count = &DashCount{Widgets: widgetCount}

	widgets, _ := r.GetWidgets(ctx, d.ID)
	d.Widgets = widgets
	return &d, nil
}

func (r *Repository) Create(ctx context.Context, orgID, userID, name string, description *string, isPublic bool, tags []string) (*Dashboard, error) {
	if tags == nil {
		tags = []string{}
	}
	row := database.Pool.QueryRow(ctx, `
		INSERT INTO dashboards (id, organization_id, user_id, name, description, is_public, tags, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, NOW(), NOW())
		RETURNING id
	`, orgID, userID, name, description, isPublic, tags)
	var id string
	if err := row.Scan(&id); err != nil {
		return nil, err
	}
	return r.FindByID(ctx, id)
}

func (r *Repository) Update(ctx context.Context, id string, data map[string]interface{}) (*Dashboard, error) {
	if name, ok := data["name"].(string); ok {
		_, _ = database.Pool.Exec(ctx, `UPDATE dashboards SET name=$1, updated_at=NOW() WHERE id=$2`, name, id)
	}
	if desc, ok := data["description"].(string); ok {
		_, _ = database.Pool.Exec(ctx, `UPDATE dashboards SET description=$1, updated_at=NOW() WHERE id=$2`, desc, id)
	}
	return r.FindByID(ctx, id)
}

func (r *Repository) Delete(ctx context.Context, id string) error {
	_, err := database.Pool.Exec(ctx, `DELETE FROM dashboards WHERE id=$1`, id)
	return err
}

func (r *Repository) CopyWidgetsFromDefault(ctx context.Context, orgID, newDashboardID string) error {
	var defaultDashID string
	err := database.Pool.QueryRow(ctx, `
		SELECT id FROM dashboards WHERE organization_id = $1 AND is_default = TRUE LIMIT 1
	`, orgID).Scan(&defaultDashID)
	if err != nil {
		return nil // no default dashboard — skip silently
	}
	_, err = database.Pool.Exec(ctx, `
		INSERT INTO dashboard_widgets
			(id, dashboard_id, machine_id, widget_type, title, layout, config, "order", created_at, updated_at)
		SELECT gen_random_uuid(), $1, machine_id, widget_type, title, layout, config, "order", NOW(), NOW()
		FROM dashboard_widgets
		WHERE dashboard_id = $2
	`, newDashboardID, defaultDashID)
	return err
}

func (r *Repository) GetWidgets(ctx context.Context, dashboardID string) ([]Widget, error) {
	rows, err := database.Pool.Query(ctx, `
		SELECT id, dashboard_id, machine_id, widget_type, title, layout, config, created_at
		FROM dashboard_widgets WHERE dashboard_id=$1 ORDER BY created_at ASC
	`, dashboardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var widgets []Widget
	machineIDs := make(map[string]struct{})
	for rows.Next() {
		var w Widget
		if err := rows.Scan(&w.ID, &w.DashboardID, &w.MachineID, &w.WidgetType, &w.Title, &w.Layout, &w.Config, &w.CreatedAt); err != nil {
			return nil, err
		}
		if w.MachineID != nil && *w.MachineID != "" {
			machineIDs[*w.MachineID] = struct{}{}
		}
		widgets = append(widgets, w)
	}

	// Fetch machine + fields for each unique machineID and attach to widgets.
	if len(machineIDs) > 0 {
		machines, _ := r.fetchMachinesWithFields(ctx, machineIDs)
		for i := range widgets {
			if widgets[i].MachineID != nil {
				if m, ok := machines[*widgets[i].MachineID]; ok {
					widgets[i].Machine = m
				}
			}
		}
	}

	return widgets, nil
}

func (r *Repository) fetchMachinesWithFields(ctx context.Context, ids map[string]struct{}) (map[string]*WidgetMachine, error) {
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}

	mRows, err := database.Pool.Query(ctx, `SELECT id, name, type FROM machines WHERE id = ANY($1::uuid[])`, idList)
	if err != nil {
		return nil, err
	}
	defer mRows.Close()

	machines := make(map[string]*WidgetMachine)
	for mRows.Next() {
		var m WidgetMachine
		if err := mRows.Scan(&m.ID, &m.Name, &m.Type); err != nil {
			continue
		}
		m.Fields = []WidgetMachineField{}
		machines[m.ID] = &m
	}

	fRows, err := database.Pool.Query(ctx, `
		SELECT machine_id, key, label, unit, threshold, upper_limit, lower_limit, precision, is_key
		FROM machine_fields WHERE machine_id = ANY($1::uuid[])
	`, idList)
	if err != nil {
		return machines, nil
	}
	defer fRows.Close()

	for fRows.Next() {
		var machineID string
		var f WidgetMachineField
		if err := fRows.Scan(&machineID, &f.Key, &f.Label, &f.Unit, &f.Threshold, &f.UpperLimit, &f.LowerLimit, &f.Precision, &f.IsKey); err != nil {
			continue
		}
		if m, ok := machines[machineID]; ok {
			m.Fields = append(m.Fields, f)
		}
	}

	return machines, nil
}

// touchDashboard bumps updated_at on the dashboard so the list page shows
// an accurate "last modified" time after any widget mutation.
func (r *Repository) touchDashboard(ctx context.Context, dashboardID string) {
	_, _ = database.Pool.Exec(ctx,
		`UPDATE dashboards SET updated_at = NOW() WHERE id = $1`, dashboardID)
}

func (r *Repository) AddWidget(ctx context.Context, dashboardID string, w Widget) (*Widget, error) {
	if w.Layout == nil {
		w.Layout = json.RawMessage("{}")
	}
	if w.Config == nil {
		w.Config = json.RawMessage("{}")
	}
	var machineID *string
	if w.MachineID != nil && *w.MachineID != "" {
		machineID = w.MachineID
	}
	row := database.Pool.QueryRow(ctx, `
		INSERT INTO dashboard_widgets (id, dashboard_id, machine_id, widget_type, title, layout, config, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, NOW(), NOW())
		RETURNING id, dashboard_id, machine_id, widget_type, title, layout, config, created_at
	`, dashboardID, machineID, w.WidgetType, w.Title, w.Layout, w.Config)

	var out Widget
	err := row.Scan(&out.ID, &out.DashboardID, &out.MachineID, &out.WidgetType, &out.Title, &out.Layout, &out.Config, &out.CreatedAt)
	if err == nil {
		r.touchDashboard(ctx, dashboardID)
	}
	return &out, err
}

func (r *Repository) UpdateWidget(ctx context.Context, widgetID string, data map[string]interface{}) error {
	if layout, ok := data["layout"]; ok {
		b, _ := json.Marshal(layout)
		_, _ = database.Pool.Exec(ctx, `UPDATE dashboard_widgets SET layout=$1, updated_at=NOW() WHERE id=$2`, string(b), widgetID)
	}
	if config, ok := data["config"]; ok {
		b, _ := json.Marshal(config)
		_, _ = database.Pool.Exec(ctx, `UPDATE dashboard_widgets SET config=$1, updated_at=NOW() WHERE id=$2`, string(b), widgetID)
	}
	if title, ok := data["title"].(string); ok {
		_, _ = database.Pool.Exec(ctx, `UPDATE dashboard_widgets SET title=$1, updated_at=NOW() WHERE id=$2`, title, widgetID)
	}
	if machineId, ok := data["machineId"].(string); ok {
		if machineId == "" {
			_, _ = database.Pool.Exec(ctx, `UPDATE dashboard_widgets SET machine_id=NULL, updated_at=NOW() WHERE id=$1`, widgetID)
		} else {
			_, _ = database.Pool.Exec(ctx, `UPDATE dashboard_widgets SET machine_id=$1, updated_at=NOW() WHERE id=$2`, machineId, widgetID)
		}
	}
	if widgetType, ok := data["widgetType"].(string); ok && widgetType != "" {
		_, _ = database.Pool.Exec(ctx, `UPDATE dashboard_widgets SET widget_type=$1, updated_at=NOW() WHERE id=$2`, widgetType, widgetID)
	}
	// Bump parent dashboard timestamp
	_, _ = database.Pool.Exec(ctx,
		`UPDATE dashboards SET updated_at = NOW()
		 WHERE id = (SELECT dashboard_id FROM dashboard_widgets WHERE id = $1)`, widgetID)
	return nil
}

func (r *Repository) BulkUpdateLayout(ctx context.Context, widgets []map[string]interface{}) error {
	widgetIDs := make([]string, 0, len(widgets))
	for _, w := range widgets {
		id, _ := w["id"].(string)
		if layout, ok := w["layout"]; ok {
			b, _ := json.Marshal(layout)
			_, _ = database.Pool.Exec(ctx, `UPDATE dashboard_widgets SET layout=$1, updated_at=NOW() WHERE id=$2`, string(b), id)
		}
		if id != "" {
			widgetIDs = append(widgetIDs, id)
		}
	}
	// Touch all affected dashboards in a single query
	if len(widgetIDs) > 0 {
		_, _ = database.Pool.Exec(ctx,
			`UPDATE dashboards SET updated_at = NOW()
			 WHERE id IN (SELECT DISTINCT dashboard_id FROM dashboard_widgets WHERE id = ANY($1))`,
			widgetIDs)
	}
	return nil
}

func (r *Repository) FindWidget(ctx context.Context, widgetID string) (*Widget, string, error) {
	row := database.Pool.QueryRow(ctx, `
		SELECT dw.id, dw.dashboard_id, dw.machine_id, dw.widget_type, dw.title, dw.layout, dw.config, dw.created_at, d.organization_id
		FROM dashboard_widgets dw
		JOIN dashboards d ON d.id = dw.dashboard_id
		WHERE dw.id = $1
	`, widgetID)

	var w Widget
	var orgID string
	err := row.Scan(&w.ID, &w.DashboardID, &w.MachineID, &w.WidgetType, &w.Title, &w.Layout, &w.Config, &w.CreatedAt, &orgID)
	return &w, orgID, err
}

func (r *Repository) DeleteWidget(ctx context.Context, widgetID string) error {
	var dashboardID string
	_ = database.Pool.QueryRow(ctx,
		`SELECT dashboard_id FROM dashboard_widgets WHERE id = $1`, widgetID).Scan(&dashboardID)
	_, err := database.Pool.Exec(ctx, `DELETE FROM dashboard_widgets WHERE id=$1`, widgetID)
	if err == nil && dashboardID != "" {
		r.touchDashboard(ctx, dashboardID)
	}
	return err
}
