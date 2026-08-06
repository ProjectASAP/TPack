"""Tests for the strawman VAE pipeline."""

import csv
import json
import os
import tempfile

import numpy as np
import pytest

from tpack_eval.baseline_tvae.flatten import FlatDataset, read_csv
from tpack_eval.baseline_tvae.vae_model import VGMTransformer, TVAE, train_tvae
from tpack_eval.baseline_tvae.reconstruct import reconstruct_and_write


def _make_csv(n_traces: int = 20, n_services: int = 3) -> str:
    """Create a minimal flat CSV for testing."""
    services = [f"svc-{i}" for i in range(n_services)]
    operations = ["GET /api", "POST /data", "db.query", "cache.get"]
    http_methods = ["GET", "POST", ""]

    fd, path = tempfile.mkstemp(suffix=".csv")
    with os.fdopen(fd, "w", newline="") as f:
        writer = csv.writer(f)
        writer.writerow([
            "trace_id", "span_id", "parent_span_id",
            "service_name", "operation_name", "span_kind", "status_code",
            "start_time_us", "duration_us",
            "depth", "parent_service_name", "parent_operation_name",
            "num_siblings", "minute_bucket",
            "http.method",
        ])

        for t in range(n_traces):
            trace_id = f"{t:032x}"
            root_id = f"{t * 10:016x}"
            child_id = f"{t * 10 + 1:016x}"
            svc = services[t % n_services]
            child_svc = services[(t + 1) % n_services]
            base_us = 1_000_000_000 + t * 1000
            bucket = base_us // 60_000_000

            writer.writerow([
                trace_id, root_id, "",
                svc, operations[t % len(operations)], "SERVER",
                "2" if t % 5 == 0 else "0",
                str(base_us), "5000",
                "0", "", "", "1", str(bucket),
                http_methods[t % len(http_methods)],
            ])
            writer.writerow([
                trace_id, child_id, root_id,
                child_svc, operations[(t + 1) % len(operations)], "CLIENT", "0",
                str(base_us + 500), "3500",
                "1", svc, operations[t % len(operations)],
                "1", str(bucket),
                "",
            ])

    return path


@pytest.fixture
def csv_path():
    path = _make_csv(n_traces=20, n_services=3)
    yield path
    os.unlink(path)


@pytest.fixture
def flat_dataset(csv_path) -> FlatDataset:
    return read_csv(csv_path)


class TestFlatten:
    def test_read_produces_correct_span_count(self, flat_dataset):
        assert flat_dataset.n_spans == 40

    def test_vocabularies_populated(self, flat_dataset):
        assert len(flat_dataset.vocabs["service_name"]) > 1
        assert flat_dataset.vocabs["service_name"][0] == "<NONE>"

    def test_root_spans_detected(self, flat_dataset):
        is_root_vocab = flat_dataset.vocabs["is_root"]
        root_idx = is_root_vocab.index("1")
        assert (flat_dataset.categoricals["is_root"] == root_idx).sum() == 20

    def test_depths_correct(self, flat_dataset):
        assert np.all(flat_dataset.continuous["depth"][:] >= 0)

    def test_duration_non_negative(self, flat_dataset):
        assert np.all(flat_dataset.continuous["duration_us"] >= 0)

    def test_extra_categoricals_detected(self, flat_dataset):
        assert "http.method" in flat_dataset.categoricals
        assert "http.method" in flat_dataset.vocabs
        vocab = flat_dataset.vocabs["http.method"]
        assert vocab[0] == "<NONE>"
        assert "GET" in vocab
        assert "POST" in vocab

    def test_continuous_columns(self, flat_dataset):
        assert set(flat_dataset.continuous.keys()) == {
            "duration_us", "start_offset_us", "depth", "num_siblings",
        }


class TestVGMTransformer:
    def test_fit_and_transform_shape(self, flat_dataset):
        t = VGMTransformer(n_modes=3)
        t.fit(flat_dataset)
        data = t.transform(flat_dataset)
        assert data.shape[0] == flat_dataset.n_spans
        assert data.shape[1] == t.output_dim
        assert not np.any(np.isnan(data))

    def test_inverse_transform_roundtrip(self, flat_dataset):
        t = VGMTransformer(n_modes=3)
        t.fit(flat_dataset)
        data = t.transform(flat_dataset)
        decoded = t.inverse_transform(data)
        accuracy = (flat_dataset.categoricals["service_name"] == decoded["service_name"]).mean()
        assert accuracy > 0.9

    def test_continuous_encoding_dims(self, flat_dataset):
        t = VGMTransformer(n_modes=3)
        t.fit(flat_dataset)
        n_cont_dims = len(flat_dataset.continuous) * (1 + 3)
        assert t.n_continuous == len(flat_dataset.continuous)
        data = t.transform(flat_dataset)
        assert data.shape[1] >= n_cont_dims


class TestTVAE:
    def test_model_forward(self, flat_dataset):
        t = VGMTransformer(n_modes=3)
        t.fit(flat_dataset)

        import torch
        cat_sizes = [t.n_modes] * t.n_continuous + [info["vocab_size"] for info in t.categorical_info]
        model = TVAE(
            data_dim=t.output_dim,
            n_continuous=t.n_continuous,
            categorical_sizes=cat_sizes,
        )
        x = torch.randn(4, t.output_dim)
        mu, logvar, cont, cats = model(x)
        assert mu.shape == (4, 16)
        assert cont.shape[1] == t.n_continuous

    def test_model_sample(self, flat_dataset):
        t = VGMTransformer(n_modes=3)
        t.fit(flat_dataset)
        cat_sizes = [t.n_modes] * t.n_continuous + [info["vocab_size"] for info in t.categorical_info]
        model = TVAE(
            data_dim=t.output_dim,
            n_continuous=t.n_continuous,
            categorical_sizes=cat_sizes,
        )
        samples = model.sample(10)
        assert samples.shape == (10, t.output_dim)
        assert not np.any(np.isnan(samples))

    def test_training(self, flat_dataset):
        t = VGMTransformer(n_modes=3)
        t.fit(flat_dataset)
        data = t.transform(flat_dataset)
        cat_sizes = [t.n_modes] * t.n_continuous + [info["vocab_size"] for info in t.categorical_info]
        model = train_tvae(
            data, t.n_continuous, cat_sizes,
            seed=42, epochs=5, batch_size=20,
        )
        samples = model.sample(5)
        assert not np.any(np.isnan(samples))

    def test_fine_tuning(self, flat_dataset):
        t = VGMTransformer(n_modes=3)
        t.fit(flat_dataset)
        data = t.transform(flat_dataset)
        cat_sizes = [t.n_modes] * t.n_continuous + [info["vocab_size"] for info in t.categorical_info]

        model = train_tvae(data, t.n_continuous, cat_sizes, seed=42, epochs=5, batch_size=20)
        model = train_tvae(data, t.n_continuous, cat_sizes, model=model, seed=43, epochs=3, batch_size=20)
        samples = model.sample(5)
        assert not np.any(np.isnan(samples))


class TestReconstruct:
    def test_reconstruct_writes_valid_otlp(self, flat_dataset):
        t = VGMTransformer(n_modes=3)
        t.fit(flat_dataset)
        data = t.transform(flat_dataset)
        cat_sizes = [t.n_modes] * t.n_continuous + [info["vocab_size"] for info in t.categorical_info]
        model = train_tvae(
            data, t.n_continuous, cat_sizes,
            seed=42, epochs=5, batch_size=20,
        )
        generated = model.sample(flat_dataset.n_spans)

        with tempfile.TemporaryDirectory() as tmpdir:
            reconstruct_and_write(generated, t, flat_dataset, tmpdir, bucket_id=0)

            files = [f for f in os.listdir(tmpdir) if f.endswith(".json")]
            assert len(files) == 1

            with open(os.path.join(tmpdir, files[0])) as f:
                data = json.load(f)

            assert "resourceSpans" in data
            total_spans = 0
            for rs in data["resourceSpans"]:
                for ss in rs["scopeSpans"]:
                    for span in ss["spans"]:
                        assert "traceId" in span
                        assert "spanId" in span
                        assert len(span["traceId"]) == 32
                        assert len(span["spanId"]) == 16
                        total_spans += 1
            assert total_spans == flat_dataset.n_spans

    def test_reconstructed_traces_have_parents(self, flat_dataset):
        t = VGMTransformer(n_modes=3)
        t.fit(flat_dataset)
        data = t.transform(flat_dataset)
        cat_sizes = [t.n_modes] * t.n_continuous + [info["vocab_size"] for info in t.categorical_info]
        model = train_tvae(
            data, t.n_continuous, cat_sizes,
            seed=42, epochs=5, batch_size=20,
        )
        generated = model.sample(flat_dataset.n_spans)

        with tempfile.TemporaryDirectory() as tmpdir:
            reconstruct_and_write(generated, t, flat_dataset, tmpdir, bucket_id=0)

            with open(os.path.join(tmpdir, os.listdir(tmpdir)[0])) as f:
                data = json.load(f)

            all_spans = []
            for rs in data["resourceSpans"]:
                for ss in rs["scopeSpans"]:
                    all_spans.extend(ss["spans"])

            with_parents = [s for s in all_spans if s.get("parentSpanId")]
            assert len(with_parents) > 0

            span_ids = {s["spanId"] for s in all_spans}
            for s in with_parents:
                assert s["parentSpanId"] in span_ids

    def test_reconstructed_spans_have_attributes(self, flat_dataset):
        t = VGMTransformer(n_modes=3)
        t.fit(flat_dataset)
        data = t.transform(flat_dataset)
        cat_sizes = [t.n_modes] * t.n_continuous + [info["vocab_size"] for info in t.categorical_info]
        model = train_tvae(
            data, t.n_continuous, cat_sizes,
            seed=42, epochs=5, batch_size=20,
        )
        generated = model.sample(flat_dataset.n_spans)

        with tempfile.TemporaryDirectory() as tmpdir:
            reconstruct_and_write(generated, t, flat_dataset, tmpdir, bucket_id=0)

            with open(os.path.join(tmpdir, os.listdir(tmpdir)[0])) as f:
                data = json.load(f)

            all_attrs = []
            for rs in data["resourceSpans"]:
                for ss in rs["scopeSpans"]:
                    for span in ss["spans"]:
                        all_attrs.extend(span.get("attributes", []))

            assert len(all_attrs) > 0, "Expected some attributes on reconstructed spans"
