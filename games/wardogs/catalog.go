package wardogs

import "context"

// CatalogEntry is one id the server knows, with its display name.
type CatalogEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// MapCatalog is GET /v1/catalog/maps.
type MapCatalog struct {
	Maps  []CatalogEntry `json:"maps"`
	Count int            `json:"count"`
}

// LightingCatalog is GET /v1/catalog/lightings.
type LightingCatalog struct {
	Lightings []CatalogEntry `json:"lightings"`
	Count     int            `json:"count"`
}

// ExperienceCatalog is GET /v1/catalog/experiences.
type ExperienceCatalog struct {
	Experiences []CatalogEntry `json:"experiences"`
	Count       int            `json:"count"`
}

// MapExperiences is GET /v1/catalog/maps/{map}/experiences: the experiences one map can run.
type MapExperiences struct {
	Map         string   `json:"map"`
	Experiences []string `json:"experiences"`
	Count       int      `json:"count"`
}

// Alternator is one of a map's zone alternators.
type Alternator struct {
	Index       int    `json:"index"`
	Tag         string `json:"tag"`
	DisplayName string `json:"displayName"`
}

// MapAlternators is GET /v1/catalog/maps/{map}/alternators.
type MapAlternators struct {
	Map         string       `json:"map"`
	Alternators []Alternator `json:"alternators"`
	Count       int          `json:"count"`
}

// CatalogMaps is the maps the server can run.
func (c *Client) CatalogMaps(ctx context.Context) (MapCatalog, error) {
	var out MapCatalog
	return out, c.call(ctx, CapCatalogMaps, nil, &out)
}

// CatalogLightings is the lighting presets.
func (c *Client) CatalogLightings(ctx context.Context) (LightingCatalog, error) {
	var out LightingCatalog
	return out, c.call(ctx, CapCatalogLightings, nil, &out)
}

// CatalogExperiences is the experiences (modes) across every map.
func (c *Client) CatalogExperiences(ctx context.Context) (ExperienceCatalog, error) {
	var out ExperienceCatalog
	return out, c.call(ctx, CapCatalogExperiences, nil, &out)
}

// MapExperiences is the experiences one map can run.
func (c *Client) MapExperiences(ctx context.Context, mapID string) (MapExperiences, error) {
	var out MapExperiences
	return out, c.call(ctx, CapMapExperiences, nil, &out, mapID)
}

// MapAlternators is one map's zone alternators.
func (c *Client) MapAlternators(ctx context.Context, mapID string) (MapAlternators, error) {
	var out MapAlternators
	return out, c.call(ctx, CapMapAlternators, nil, &out, mapID)
}
