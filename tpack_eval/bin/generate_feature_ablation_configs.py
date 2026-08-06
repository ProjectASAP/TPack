#!/usr/bin/env python3
"""Generate leave-one-out feature ablation configs for otel-demo.

Reads the base config (configs/otel_demo.yaml) and generates 22 configs
under configs/ablation/, each removing one feature column and moving it to
dependent_attributes.

Usage:
    uv run generate_feature_ablation_configs
"""

import yaml
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent
BASE_CONFIG = ROOT / "configs" / "otel_demo.yaml"
OUTPUT_DIR = ROOT / "configs" / "ablation"


def main():
    with open(BASE_CONFIG) as f:
        base = yaml.safe_load(f)

    features = base["primary_attributes"]
    metadata = base.get("dependent_attributes", [])

    OUTPUT_DIR.mkdir(parents=True, exist_ok=True)

    for col in features:
        # Sanitize column name for filename (replace dots with underscores)
        safe_col = col.replace(".", "_")
        config_name = f"otel_demo_no_{safe_col}"

        new_features = [f for f in features if f != col]
        new_metadata = sorted(set(metadata + [col]))

        cfg = {
            "name": config_name,
            "primary_attributes": new_features,
            "dependent_attributes": new_metadata,
            "metadata_predictor": base.get("metadata_predictor", "statistical"),
            "offset_value": base.get("offset_value", "ratio"),
            "offset_model": base.get("offset_model", "regression"),
        }

        out_path = OUTPUT_DIR / f"{config_name}.yaml"
        with open(out_path, "w") as f:
            yaml.dump(cfg, f, default_flow_style=False, sort_keys=False)

        print(f"  {out_path.name}: {len(new_features)} features (removed {col})")

    print(f"\nGenerated {len(features)} configs in {OUTPUT_DIR}")


if __name__ == "__main__":
    main()
