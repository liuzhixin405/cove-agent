package config

// profileValues are the settings a saved profile records (SnapshotProfile),
// as they stood at one point of Load.
type profileValues struct {
	model, modelFast                       string
	providerName, providerKey, providerURL string
	permissionMode, systemPrompt           string
	maxBudgetUsd                           float64
	thinkingTokens                         int
	imageFilesAPI                          bool
}

func profileValuesOf(c *Config) profileValues {
	return profileValues{
		model: c.Model, modelFast: c.ModelFast,
		providerName: c.Provider.Name, providerKey: c.Provider.APIKey, providerURL: c.Provider.BaseURL,
		permissionMode: c.PermissionMode, systemPrompt: c.SystemPrompt,
		maxBudgetUsd:   c.MaxBudgetUsd,
		thinkingTokens: c.ThinkingTokens,
		imageFilesAPI:  c.Provider.ImageFilesEnabled(),
	}
}

// recordUserLevelValues is called by Load once cfg is complete. base is the
// config as it was before the project .cove.json was merged in; it gets the
// same profile and defaults cfg got, so base and cfg differ exactly in what
// the project set.
func recordUserLevelValues(cfg *Config, base Config, prof *Profile) {
	applyProfile(&base, prof)
	applyDefaults(&base)
	user, loaded := profileValuesOf(&base), profileValuesOf(cfg)
	if user == loaded {
		cfg.userValues, cfg.loadedValues = nil, nil
		return
	}
	cfg.userValues, cfg.loadedValues = &user, &loaded
}

// SnapshotProfile returns the current settings as a profile for /profile
// save. It used to be built from the live config, so what a trusted project
// .cove.json set — its provider base_url and api_key, system prompt,
// permission mode, model, budget — was written into the global profiles and
// came along to every other project on `/profile switch`: that project's
// proxy then received the user's global key. A setting that still holds the
// value the project gave it is recorded with the user's own value instead;
// one the user changed during the session is recorded as it is now.
func (c *Config) SnapshotProfile() *Profile {
	v := profileValuesOf(c)
	if c.userValues != nil && c.loadedValues != nil {
		u, l := *c.userValues, *c.loadedValues
		str := func(live *string, loaded, user string) {
			if *live == loaded && loaded != user {
				*live = user
			}
		}
		str(&v.model, l.model, u.model)
		str(&v.modelFast, l.modelFast, u.modelFast)
		str(&v.providerName, l.providerName, u.providerName)
		str(&v.providerKey, l.providerKey, u.providerKey)
		str(&v.providerURL, l.providerURL, u.providerURL)
		if v.imageFilesAPI == l.imageFilesAPI && l.imageFilesAPI != u.imageFilesAPI {
			v.imageFilesAPI = u.imageFilesAPI
		}
		str(&v.permissionMode, l.permissionMode, u.permissionMode)
		str(&v.systemPrompt, l.systemPrompt, u.systemPrompt)
		if v.maxBudgetUsd == l.maxBudgetUsd && l.maxBudgetUsd != u.maxBudgetUsd {
			v.maxBudgetUsd = u.maxBudgetUsd
		}
		if v.thinkingTokens == l.thinkingTokens && l.thinkingTokens != u.thinkingTokens {
			v.thinkingTokens = u.thinkingTokens
		}
	}
	// Copies, not &c.Debug: pointing the profile at the live config would
	// let a later /debug toggle silently rewrite the saved profile.
	debug, verbose := c.Debug, c.Verbose
	imageFilesAPI := v.imageFilesAPI
	return &Profile{
		Model:          v.model,
		ModelFast:      v.modelFast,
		Provider:       &ProviderConfig{Name: v.providerName, APIKey: v.providerKey, BaseURL: v.providerURL, ImageFilesAPI: &imageFilesAPI},
		PermissionMode: v.permissionMode,
		MaxBudgetUsd:   v.maxBudgetUsd,
		ThinkingTokens: v.thinkingTokens,
		// Explicit pointers so the profile records the current state
		// faithfully, including off.
		Debug:        &debug,
		Verbose:      &verbose,
		SystemPrompt: v.systemPrompt,
	}
}
