import orjson
from dataclasses import dataclass


@dataclass
class CostConfig:
    transmission_per_gb: float = 0.1
    gpu_per_hour: float = 0.38
    cpu_per_hour: float = 0.16


@dataclass
class ExperimentData:
    name: str
    compressor_key: str
    experiment_name: str
    iteration: str
    size_kb: float
    cpu_time_seconds: float
    gpu_time_seconds: float
    transmission_cost: float
    compute_cost: float
    total_cost: float
    is_head_sampling: bool = False


class ReportParser:
    def __init__(self, cost_config: CostConfig | None = None):
        self.cost_config = cost_config or CostConfig()

    def parse_report(self, report_path: str) -> list[ExperimentData]:
        with open(report_path, "rb") as f:
            report_data = orjson.loads(f.read())
        return self.parse_report_data(report_data)

    def parse_report_data(self, report_data: dict) -> list[ExperimentData]:
        """Parse experiments from already-loaded report data (avoids re-reading file)."""
        experiments = []

        # Extract compressor names from any report section
        compressor_names = self._get_compressor_names(report_data)

        for compressor_name in compressor_names:
            try:
                experiment = self._parse_compressor(report_data, compressor_name)
                if experiment:
                    experiments.append(experiment)
            except (KeyError, ValueError, TypeError) as e:
                print(f"Warning: Failed to parse {compressor_name}: {e}")

        return experiments

    def _get_compressor_names(self, report_data: dict) -> list[str]:
        compressor_names = set()

        # Get from reports section
        if "reports" in report_data:
            for report_content in report_data["reports"].values():
                if "compressors" in report_content:
                    compressor_names.update(report_content["compressors"])
                elif isinstance(report_content, dict):
                    compressor_names.update(report_content.keys())

        return list(compressor_names)

    def _parse_compressor(
        self, report_data: dict, compressor_name: str
    ) -> ExperimentData | None:
        size_data = self._extract_size_data(report_data, compressor_name)
        time_data = self._extract_time_data(report_data, compressor_name)

        name_parts = self._parse_compressor_name(compressor_name)
        if not name_parts:
            return None

        if not all([size_data, time_data]):
            print(f"Warning: Missing size or time data for {compressor_name}")
            return None

        # Calculate costs
        cost_data = self._calculate_costs(
            size_data["size_bytes"], time_data["cpu_seconds"], time_data["gpu_seconds"]
        )

        return ExperimentData(
            name=name_parts["display_name"],
            compressor_key=compressor_name,
            experiment_name=name_parts["experiment_name"],
            iteration=name_parts["iteration"],
            size_kb=size_data["size_bytes"] / 1024,
            cpu_time_seconds=time_data["cpu_seconds"],
            gpu_time_seconds=time_data["gpu_seconds"],
            transmission_cost=cost_data["transmission_cost"],
            compute_cost=cost_data["compute_cost"],
            total_cost=cost_data["total_cost"],
            is_head_sampling=name_parts["is_head_sampling"],
        )

    def _parse_compressor_name(self, compressor_name: str) -> dict | None:
        """Parse compressor name generically.

        Format: {app_prefix}_{approach}_{iteration}
        where approach may contain underscores (e.g., "head_50", "tvae", "tpack_feat22").
        The iteration is always the last segment and is a digit.
        The app prefix is derived from the report metadata (all keys share it).
        """
        parts = compressor_name.split("_")
        if not parts:
            return None

        has_iteration = parts[-1].isdigit()
        iteration = parts[-1] if has_iteration else "1"

        # Find where the approach name starts by matching known keywords.
        # Keywords mark the start of the approach segment.
        keywords = {"head", "tpack", "tvae", "tail", "hindsight", "sifter"}
        approach_start = None
        for i, p in enumerate(parts):
            if p in keywords:
                approach_start = i
                break
        if approach_start is None:
            return None

        # Everything from approach_start to end (excluding iteration digit if present).
        if has_iteration:
            approach_parts = parts[approach_start:-1]  # exclude iteration
        else:
            approach_parts = parts[approach_start:]
        if not approach_parts:
            return None

        # Head sampling: head_{rate} -> display "1:{rate}", experiment = rate
        if approach_parts[0] == "head" and len(approach_parts) >= 2:
            sampling_rate = approach_parts[1]
            experiment_name = sampling_rate
            # Extra parts after rate are sub-experiment (e.g., head_50_weighted)
            if len(approach_parts) > 2:
                experiment_name = "_".join(approach_parts[1:])
            return {
                "display_name": f"1:{sampling_rate}",
                "experiment_name": experiment_name,
                "iteration": iteration,
                "is_head_sampling": True,
            }

        # TPack: tpack -> "TPack", tpack_{variant} -> "TPack {variant}"
        if approach_parts[0] == "tpack":
            if len(approach_parts) == 1:
                display = "TPack"
                experiment_name = "tpack"
            else:
                variant = "_".join(approach_parts[1:])
                display = f"TPack {variant}"
                experiment_name = variant
            return {
                "display_name": display,
                "experiment_name": experiment_name,
                "iteration": iteration,
                "is_head_sampling": False,
            }

        # Sifter: sifter_{rate} -> "Sifter 1:{rate}", experiment_name = "sifter_{rate}"
        if approach_parts[0] == "sifter" and len(approach_parts) >= 2:
            rate = approach_parts[1]
            return {
                "display_name": f"Sifter 1:{rate}",
                "experiment_name": f"sifter_{rate}",
                "iteration": iteration,
                "is_head_sampling": False,
            }

        # Tail/Hindsight: single-instance approaches (no iteration)
        if approach_parts[0] in ("tail", "hindsight"):
            display = approach_parts[0].capitalize()
            return {
                "display_name": display,
                "experiment_name": approach_parts[0],
                "iteration": iteration,
                "is_head_sampling": False,
            }

        # Generic: join approach parts for display and experiment name
        approach_name = "_".join(approach_parts)
        return {
            "display_name": approach_name,
            "experiment_name": approach_name,
            "iteration": iteration,
            "is_head_sampling": False,
        }

    def _extract_size_data(
        self, report_data: dict, compressor_name: str
    ) -> dict | None:
        try:
            size_report = report_data["reports"]["size"]
            if compressor_name in size_report:
                size_bytes = size_report[compressor_name]["size"]["avg"]
                return {"size_bytes": size_bytes}
        except KeyError:
            pass

        return None

    def _extract_time_data(
        self, report_data: dict, compressor_name: str
    ) -> dict | None:
        try:
            time_report = report_data["reports"]["time"]
            if compressor_name in time_report:
                data = time_report[compressor_name]
                comp_cpu = data.get("compression_cpu_time_seconds", {}).get("avg", 0)
                comp_gpu = data.get("compression_gpu_time_seconds", {}).get("avg", 0)
                decomp_cpu = data.get("decompression_cpu_time_seconds", {}).get("avg", 0)
                decomp_gpu = data.get("decompression_gpu_time_seconds", {}).get("avg", 0)
                return {
                    "cpu_seconds": comp_cpu + decomp_cpu,
                    "gpu_seconds": comp_gpu + decomp_gpu,
                }
        except KeyError:
            pass

        return None

    def _calculate_costs(
        self, size_bytes: float, cpu_seconds: float, gpu_seconds: float
    ) -> dict:
        # Size conversion
        size_gb = size_bytes / (1024**3)
        transmission_cost = size_gb * self.cost_config.transmission_per_gb

        # Time conversion
        cpu_cost = (cpu_seconds / 3600) * self.cost_config.cpu_per_hour
        gpu_cost = (gpu_seconds / 3600) * self.cost_config.gpu_per_hour
        compute_cost = cpu_cost + gpu_cost

        total_cost = transmission_cost + compute_cost

        return {
            "transmission_cost": transmission_cost,
            "compute_cost": compute_cost,
            "total_cost": total_cost,
        }
