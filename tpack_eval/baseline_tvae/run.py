"""TVAE baseline pipeline — per-bucket training with fine-tuning.

Stage 1 (train+generate): Train TVAE per bucket, generate, save .npz
    uv run tvae_train --input spans_flat.csv --output dir/ --seed 42

Stage 2 (reconstruct): Load .npz, reconstruct traces, write OTLP JSON
    uv run tvae_reconstruct --input spans_flat.csv --generated dir/ --output otlp_dir/
"""

import argparse
import os
import pickle
import time

import numpy as np

from tpack_eval.baseline_tvae.flatten import FlatDataset, read_csv
from tpack_eval.baseline_tvae.metadata_predictor import StatisticalMetadataPredictor
from tpack_eval.baseline_tvae.reconstruct import reconstruct_and_write
from tpack_eval.baseline_tvae.vae_model import VGMTransformer, TVAE, train_tvae


def _bucket_dataset(dataset: FlatDataset) -> dict[int, FlatDataset]:
    """Split dataset by minute_bucket."""
    buckets: dict[int, list[int]] = {}
    for i in range(dataset.n_spans):
        b = int(dataset.minute_bucket[i])
        if b not in buckets:
            buckets[b] = []
        buckets[b].append(i)

    result = {}
    for b, indices in sorted(buckets.items()):
        idx = np.array(indices)
        result[b] = FlatDataset(
            continuous={k: v[idx] for k, v in dataset.continuous.items()},
            categoricals={k: v[idx] for k, v in dataset.categoricals.items()},
            vocabs=dataset.vocabs,
            minute_bucket=dataset.minute_bucket[idx],
        )
    return result


def main():
    """Stage 1: per-bucket train + generate → save .npz per bucket."""
    parser = argparse.ArgumentParser(description="Strawman TVAE: per-bucket train and generate")
    parser.add_argument("--input", required=True, help="Input CSV file (from tpack-eval --flatten-csv)")
    parser.add_argument("--output", required=True, help="Output directory for generated .npz files")
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--epochs", type=int, default=20, help="Epochs for first bucket (fine-tune uses same)")
    parser.add_argument("--batch-size", type=int, default=500)
    parser.add_argument("--device", default="cpu", help="torch device (cpu or cuda)")
    args = parser.parse_args()

    os.makedirs(args.output, exist_ok=True)

    # Step 1: Read CSV
    print("=" * 60)
    print("Step 1: Reading flattened CSV")
    print("=" * 60)
    t0 = time.time()
    dataset = read_csv(args.input)
    print(f"  {dataset.n_spans} spans in {time.time() - t0:.1f}s")

    # Step 2: Fit transformer on full dataset (shared vocabs)
    print("\n" + "=" * 60)
    print("Step 2: Fitting transformer")
    print("=" * 60)
    t0 = time.time()
    transformer = VGMTransformer(n_modes=3)
    transformer.fit(dataset)
    print(f"  output_dim={transformer.output_dim} in {time.time() - t0:.1f}s")

    # Fit metadata predictor for high-cardinality columns
    print("\n" + "=" * 60)
    print("Step 2b: Fitting metadata predictor")
    print("=" * 60)
    t0 = time.time()
    metadata_predictor = StatisticalMetadataPredictor()
    metadata_predictor.fit(dataset)
    print(f"  in {time.time() - t0:.1f}s")

    # Save transformer + metadata predictor
    meta_path = os.path.join(args.output, "meta.pkl")
    with open(meta_path, "wb") as f:
        pickle.dump({"transformer": transformer, "metadata_predictor": metadata_predictor}, f)

    # Step 3: Split by bucket and train/generate per bucket
    print("\n" + "=" * 60)
    print("Step 3: Per-bucket training and generation")
    print("=" * 60)
    bucket_datasets = _bucket_dataset(dataset)
    print(f"  {len(bucket_datasets)} buckets")

    n_continuous = transformer.n_continuous
    categorical_sizes = (
        [transformer.n_modes] * transformer.n_continuous
        + [info["vocab_size"] for info in transformer.categorical_info]
    )

    import io as io_mod
    import gzip as gzip_mod
    import torch
    model = None
    total_transform_time = 0.0
    total_train_time = 0.0
    total_gen_time = 0.0
    total_gz_compress_time = 0.0
    total_gz_decompress_time = 0.0

    model_dir = os.path.join(args.output, "compressed", "data")
    os.makedirs(model_dir, exist_ok=True)

    for i, (bucket_id, bucket_ds) in enumerate(sorted(bucket_datasets.items())):
        t0 = time.time()
        data = transformer.transform(bucket_ds)
        transform_time = time.time() - t0
        total_transform_time += transform_time

        t0 = time.time()
        is_first = (model is None)
        model = train_tvae(
            data=data,
            n_continuous=n_continuous,
            categorical_sizes=categorical_sizes,
            model=None if is_first else model,
            seed=args.seed + bucket_id,
            epochs=args.epochs,
            batch_size=args.batch_size,
            lr=1e-3,
            device_str=args.device,
        )
        train_time = time.time() - t0
        total_train_time += train_time

        t0 = time.time()
        device = torch.device(args.device)
        generated = model.sample(bucket_ds.n_spans, device=device)
        gen_time = time.time() - t0
        total_gen_time += gen_time

        npz_path = os.path.join(args.output, f"bucket_{bucket_id}.npz")
        np.savez_compressed(npz_path, generated=generated)

        # Save model weights to compressed/data/ (gzip to match TPack)
        # Serialize to memory buffer (not timed — analogous to Marshal())
        buf = io_mod.BytesIO()
        torch.save(model.state_dict(), buf)
        raw_bytes = buf.getvalue()

        # Gzip compress (timed, in-memory only)
        t_gz = time.time()
        compressed = gzip_mod.compress(raw_bytes)
        total_gz_compress_time += time.time() - t_gz

        # Gzip decompress benchmark (timed, in-memory only)
        t_gz = time.time()
        gzip_mod.decompress(compressed)
        total_gz_decompress_time += time.time() - t_gz

        # Write to disk (not timed)
        with open(os.path.join(model_dir, f"model_bucket_{bucket_id}"), "wb") as f:
            f.write(compressed)

        label = "train" if is_first else "fine-tune"
        print(f"  Bucket {bucket_id} ({i + 1}/{len(bucket_datasets)}): "
              f"{bucket_ds.n_spans} spans, encode {transform_time:.1f}s, {label} {train_time:.1f}s, gen {gen_time:.1f}s")

    print(f"\n  Total: encode {total_transform_time:.1f}s, train {total_train_time:.1f}s, gen {total_gen_time:.1f}s")
    print(f"  Gzip: compress {total_gz_compress_time:.2f}s, decompress {total_gz_decompress_time:.2f}s")
    print(f"  Output: {args.output}")

    # Copy meta.pkl (VGM transformer) into compressed/data/ (gzip, timed)
    with open(meta_path, "rb") as f_in:
        meta_bytes = f_in.read()
    t_gz = time.time()
    meta_compressed = gzip_mod.compress(meta_bytes)
    total_gz_compress_time += time.time() - t_gz
    t_gz = time.time()
    gzip_mod.decompress(meta_compressed)
    total_gz_decompress_time += time.time() - t_gz
    with open(os.path.join(model_dir, "meta.pkl"), "wb") as f_out:
        f_out.write(meta_compressed)

    # Write 4 canonical timing files (compression/decompression × cpu/gpu)
    comp_cpu = total_transform_time + total_gz_compress_time    # agent-side CPU
    comp_gpu = total_train_time                                  # agent-side GPU
    decomp_cpu = total_gz_decompress_time                        # backend-side CPU
    decomp_gpu = total_gen_time                                  # backend-side GPU
    for name, val in [
        ("compression_cpu_time_seconds", comp_cpu),
        ("compression_gpu_time_seconds", comp_gpu),
        ("decompression_cpu_time_seconds", decomp_cpu),
        ("decompression_gpu_time_seconds", decomp_gpu),
    ]:
        with open(os.path.join(model_dir, name), "w") as f:
            f.write(f"{val:.6f}")
    total_time = comp_cpu + comp_gpu + decomp_cpu + decomp_gpu
    print(f"  Timing: comp_cpu {comp_cpu:.1f}s, comp_gpu {comp_gpu:.1f}s, decomp_cpu {decomp_cpu:.2f}s, decomp_gpu {decomp_gpu:.1f}s, total {total_time:.1f}s")


def main_reconstruct():
    """Stage 2: load .npz per bucket → reconstruct traces → write OTLP JSON."""
    parser = argparse.ArgumentParser(description="Strawman TVAE: reconstruct traces")
    parser.add_argument("--input", required=True, help="Input CSV file (for dataset vocabs)")
    parser.add_argument("--generated", required=True, help="Directory with bucket_*.npz files")
    parser.add_argument("--output", required=True, help="Output OTLP JSON directory")
    args = parser.parse_args()

    # Step 1: Load dataset for vocabs
    print("=" * 60)
    print("Step 1: Loading dataset for vocabs")
    print("=" * 60)
    t0 = time.time()
    dataset = read_csv(args.input)
    print(f"  {dataset.n_spans} spans in {time.time() - t0:.1f}s")

    # Step 2: Load transformer
    print("\n" + "=" * 60)
    print("Step 2: Loading transformer")
    print("=" * 60)
    meta_path = os.path.join(args.generated, "meta.pkl")
    with open(meta_path, "rb") as f:
        meta = pickle.load(f)
    transformer = meta["transformer"]
    metadata_predictor = meta.get("metadata_predictor")
    print(f"  output_dim={transformer.output_dim}")
    if metadata_predictor and metadata_predictor.columns:
        print(f"  metadata predictor: {metadata_predictor.columns}")

    # Step 3: Reconstruct per bucket
    print("\n" + "=" * 60)
    print("Step 3: Reconstructing traces")
    print("=" * 60)
    os.makedirs(args.output, exist_ok=True)
    t0 = time.time()
    total_spans = 0

    npz_files = sorted(f for f in os.listdir(args.generated) if f.startswith("bucket_") and f.endswith(".npz"))
    for fi, fname in enumerate(npz_files):
        bucket_id = int(fname.replace("bucket_", "").replace(".npz", ""))
        t1 = time.time()
        data = np.load(os.path.join(args.generated, fname))
        generated = data["generated"]

        n_svcs = reconstruct_and_write(generated, transformer, dataset, args.output, bucket_id=bucket_id, metadata_predictor=metadata_predictor)
        total_spans += generated.shape[0]
        print(f"  Bucket {bucket_id} ({fi+1}/{len(npz_files)}): "
              f"{generated.shape[0]} spans, {n_svcs} services, {time.time()-t1:.1f}s")

    print(f"\n  Reconstructed {total_spans} spans in {time.time() - t0:.1f}s")
    print(f"Output: {args.output}")


if __name__ == "__main__":
    main()
