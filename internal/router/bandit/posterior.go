package bandit

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
)

// Arm is the Gaussian posterior over reward for one (cluster, model) cell.
type Arm struct {
	Mean     float64
	Variance float64
}

// Posterior holds per-cluster, per-model reward posteriors produced by
// train_thompson_posterior.py (ts_posterior.json).
type Posterior struct {
	cells map[int]map[string]Arm
}

type posteriorFile struct {
	Posterior map[string]map[string]armJSON `json:"posterior"`
}

type armJSON struct {
	Mean     float64 `json:"mean"`
	Variance float64 `json:"variance"`
}

// LoadPosterior reads ts_posterior.json from path.
func LoadPosterior(path string) (*Posterior, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bandit: read posterior %q: %w", path, err)
	}
	var file posteriorFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("bandit: parse posterior %q: %w", path, err)
	}
	if len(file.Posterior) == 0 {
		return nil, fmt.Errorf("bandit: posterior %q has no cells", path)
	}

	cells := make(map[int]map[string]Arm, len(file.Posterior))
	for cidStr, arms := range file.Posterior {
		cid, err := strconv.Atoi(cidStr)
		if err != nil {
			return nil, fmt.Errorf("bandit: posterior cluster id %q: %w", cidStr, err)
		}
		cell := make(map[string]Arm, len(arms))
		for model, a := range arms {
			if a.Variance < 0 {
				return nil, fmt.Errorf("bandit: negative variance for cluster %d model %q", cid, model)
			}
			cell[model] = Arm{Mean: a.Mean, Variance: a.Variance}
		}
		cells[cid] = cell
	}
	return &Posterior{cells: cells}, nil
}

// Sample draws a Thompson sample for model across the given cluster ids.
// When no arm exists in any cluster, ok is false.
func (p *Posterior) Sample(clusterIDs []int, model string, norm func() float64) (sample float64, ok bool) {
	if p == nil || len(clusterIDs) == 0 {
		return 0, false
	}
	best := -1.0
	found := false
	for _, cid := range clusterIDs {
		cell, okCell := p.cells[cid]
		if !okCell {
			continue
		}
		arm, okArm := cell[model]
		if !okArm {
			continue
		}
		draw := arm.Mean
		if arm.Variance > 0 {
			draw += norm() * math.Sqrt(arm.Variance)
		}
		if !found || draw > best {
			best = draw
			found = true
		}
	}
	return best, found
}
