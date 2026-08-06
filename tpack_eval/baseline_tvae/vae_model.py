"""TVAE model following Xu et al. (NeurIPS 2019), Section 4.5.

Uses VGM mode-specific normalization (Section 4.2) with 3 modes.
"""

import copy
import time as _time

import numpy as np
import torch
import torch.nn as nn
import torch.nn.functional as F
from sklearn.mixture import BayesianGaussianMixture

from tpack_eval.baseline_tvae.flatten import FlatDataset


class VGMTransformer:
    """VGM mode-specific normalization (Xu et al. Section 4.2).

    For each continuous column, fits a Bayesian Gaussian Mixture with n_modes
    components. Each value is encoded as α (normalized within its mode) + β
    (one-hot mode indicator). Categorical columns use standard one-hot.
    """

    def __init__(self, n_modes: int = 3, max_samples: int = 50_000):
        self.n_modes = n_modes
        self.max_samples = max_samples
        self.continuous_info: list[dict] = []   # ordered list: {name, means, stds, weights}
        self.categorical_info: list[dict] = []  # ordered list: {name, vocab_size}
        self.n_continuous = 0
        self.output_dim = 0
        # Ordered column names (set during fit, used by transform)
        self._cont_names: list[str] = []
        self._cat_names: list[str] = []

    def fit(self, dataset: FlatDataset):
        # Subsample for BGM fitting
        n = dataset.n_spans
        if n > self.max_samples:
            rng = np.random.RandomState(42)
            idx = rng.choice(n, self.max_samples, replace=False)
        else:
            idx = np.arange(n)

        # Fit continuous columns
        self._cont_names = sorted(dataset.continuous.keys())
        dim = 0
        self.continuous_info = []
        for ci, name in enumerate(self._cont_names):
            t0 = _time.time()
            values = dataset.continuous[name]
            if name == "duration_us":
                values = np.log1p(values)

            sample = values[idx].reshape(-1, 1).astype(np.float64)
            bgm = BayesianGaussianMixture(
                n_components=self.n_modes,
                max_iter=200,
                random_state=42,
                weight_concentration_prior=0.01,
            )
            bgm.fit(sample)

            means = bgm.means_.flatten()
            stds = np.maximum(np.sqrt(bgm.covariances_.flatten()), 1e-6)
            print(f"    VGM {ci+1}/{len(self._cont_names)}: {name} in {_time.time()-t0:.1f}s")

            self.continuous_info.append({
                "name": name,
                "means": means,
                "stds": stds,
                "weights": bgm.weights_,
            })
            dim += 1 + self.n_modes  # α + β

        self.n_continuous = len(self.continuous_info)

        # Fit categorical columns
        self._cat_names = sorted(dataset.vocabs.keys())
        self.categorical_info = []
        for name in self._cat_names:
            vs = len(dataset.vocabs[name])
            self.categorical_info.append({"name": name, "vocab_size": vs})
            dim += vs

        self.output_dim = dim

    def _encode_continuous(self, values: np.ndarray, info: dict) -> tuple[np.ndarray, np.ndarray]:
        """Encode a continuous column using mode-specific normalization."""
        means = info["means"]
        stds = info["stds"]
        n = len(values)

        log_probs = np.zeros((n, self.n_modes))
        for k in range(self.n_modes):
            log_probs[:, k] = (
                np.log(info["weights"][k] + 1e-10)
                - 0.5 * ((values - means[k]) / stds[k]) ** 2
                - np.log(stds[k])
            )

        modes = np.argmax(log_probs, axis=1)

        alpha = np.zeros(n, dtype=np.float32)
        for k in range(self.n_modes):
            mask = modes == k
            if mask.any():
                alpha[mask] = (values[mask] - means[k]) / (4.0 * stds[k])
        alpha = np.clip(alpha, -1.0, 1.0)

        beta = np.zeros((n, self.n_modes), dtype=np.float32)
        beta[np.arange(n), modes] = 1.0

        return alpha.reshape(-1, 1), beta

    def transform(self, dataset: FlatDataset) -> np.ndarray:
        """Encode dataset. Layout: [α_1..α_C, β_1..β_C, cat_1..cat_K]."""
        alpha_parts: list[np.ndarray] = []
        beta_parts: list[np.ndarray] = []

        for info, name in zip(self.continuous_info, self._cont_names):
            values = dataset.continuous[name]
            if name == "duration_us":
                values = np.log1p(values)
            alpha, beta = self._encode_continuous(values.astype(np.float64), info)
            alpha_parts.append(alpha)
            beta_parts.append(beta)

        cat_parts: list[np.ndarray] = []
        n = dataset.n_spans
        for info, name in zip(self.categorical_info, self._cat_names):
            indices = dataset.categoricals[name]
            one_hot = np.zeros((n, info["vocab_size"]), dtype=np.float32)
            valid = (indices >= 0) & (indices < info["vocab_size"])
            one_hot[valid, indices[valid]] = 1.0
            cat_parts.append(one_hot)

        return np.concatenate(alpha_parts + beta_parts + cat_parts, axis=1).astype(np.float32)

    def inverse_transform(self, data: np.ndarray) -> dict:
        """Decode from layout: [α_1..α_C, β_1..β_C, cat_1..cat_K]."""
        result: dict[str, np.ndarray] = {}
        nc = self.n_continuous

        alphas = data[:, :nc]
        col = nc
        betas = []
        for _ in range(nc):
            betas.append(data[:, col: col + self.n_modes])
            col += self.n_modes

        for i, info in enumerate(self.continuous_info):
            alpha = alphas[:, i]
            modes = np.argmax(betas[i], axis=1)
            values = np.zeros(len(alpha), dtype=np.float64)
            for k in range(self.n_modes):
                mask = modes == k
                if mask.any():
                    values[mask] = alpha[mask] * 4.0 * info["stds"][k] + info["means"][k]
            if info["name"] == "duration_us":
                values = np.expm1(values)
                values = np.maximum(values, 0)
            result[info["name"]] = values

        for info in self.categorical_info:
            vs = info["vocab_size"]
            logits = data[:, col: col + vs]
            col += vs
            result[info["name"]] = np.argmax(logits, axis=1)

        return result


class TVAE(nn.Module):
    """Tabular VAE — smaller architecture for per-bucket training."""

    def __init__(
        self,
        data_dim: int,
        n_continuous: int,
        categorical_sizes: list[int],
        latent_dim: int = 16,
        hidden_dim: int = 32,
    ):
        super().__init__()
        self.data_dim = data_dim
        self.latent_dim = latent_dim
        self.n_continuous = n_continuous
        self.categorical_sizes = categorical_sizes

        self.encoder = nn.Sequential(
            nn.Linear(data_dim, hidden_dim),
            nn.ReLU(),
            nn.Linear(hidden_dim, hidden_dim),
            nn.ReLU(),
        )
        self.fc_mu = nn.Linear(hidden_dim, latent_dim)
        self.fc_logvar = nn.Linear(hidden_dim, latent_dim)

        self.decoder = nn.Sequential(
            nn.Linear(latent_dim, hidden_dim),
            nn.ReLU(),
            nn.Linear(hidden_dim, hidden_dim),
            nn.ReLU(),
        )

        self.continuous_head = nn.Linear(hidden_dim, n_continuous)
        self.continuous_logvar = nn.Parameter(torch.zeros(n_continuous))
        self.categorical_heads = nn.ModuleList(
            [nn.Linear(hidden_dim, vs) for vs in categorical_sizes]
        )

    def encode(self, x: torch.Tensor):
        h = self.encoder(x)
        return self.fc_mu(h), self.fc_logvar(h)

    def reparameterize(self, mu: torch.Tensor, logvar: torch.Tensor):
        return mu + torch.randn_like(mu) * torch.exp(0.5 * logvar)

    def decode(self, z: torch.Tensor):
        h = self.decoder(z)
        cont = self.continuous_head(h)
        cats = [head(h) for head in self.categorical_heads]
        return cont, cats

    def forward(self, x: torch.Tensor):
        mu, logvar = self.encode(x)
        z = self.reparameterize(mu, logvar)
        cont, cats = self.decode(z)
        return mu, logvar, cont, cats

    def loss(self, x: torch.Tensor, mu, logvar, cont_pred, cat_preds):
        cont_target = x[:, :self.n_continuous]
        nll = 0.5 * (self.continuous_logvar + (cont_target - cont_pred) ** 2 / torch.exp(self.continuous_logvar))
        recon = nll.sum(dim=1).mean()

        col = self.n_continuous
        for i, vs in enumerate(self.categorical_sizes):
            target = x[:, col: col + vs].argmax(dim=1)
            recon = recon + F.cross_entropy(cat_preds[i], target)
            col += vs

        kl = -0.5 * torch.mean(1 + logvar - mu.pow(2) - logvar.exp())
        return recon + kl, {"recon": recon.item(), "kl": kl.item()}

    @torch.no_grad()
    def sample(self, n: int, device: torch.device = torch.device("cpu")) -> np.ndarray:
        self.eval()
        z = torch.randn(n, self.latent_dim, device=device)
        cont, cats = self.decode(z)

        std = torch.exp(0.5 * self.continuous_logvar).cpu().numpy()
        cont_np = cont.cpu().numpy() + np.random.randn(n, self.n_continuous) * std

        cat_parts = []
        for i, vs in enumerate(self.categorical_sizes):
            probs = F.softmax(cats[i], dim=1).cpu().numpy()
            indices = np.array([np.random.choice(vs, p=p) for p in probs])
            one_hot = np.zeros((n, vs), dtype=np.float32)
            one_hot[np.arange(n), indices] = 1.0
            cat_parts.append(one_hot)

        return np.concatenate([cont_np] + cat_parts, axis=1).astype(np.float32)


def train_tvae(
    data: np.ndarray,
    n_continuous: int,
    categorical_sizes: list[int],
    model: TVAE | None = None,
    seed: int = 42,
    epochs: int = 20,
    batch_size: int = 500,
    lr: float = 1e-3,
    device_str: str = "cpu",
) -> TVAE:
    """Train TVAE on transformed data. If model is provided, fine-tune it."""
    torch.manual_seed(seed)
    np.random.seed(seed)
    device = torch.device(device_str)

    data_dim = data.shape[1]

    if model is None:
        model = TVAE(
            data_dim=data_dim,
            n_continuous=n_continuous,
            categorical_sizes=categorical_sizes,
            latent_dim=16,
            hidden_dim=32,
        )
    model = model.to(device)

    optimizer = torch.optim.Adam(model.parameters(), lr=lr)
    data_tensor = torch.tensor(data, dtype=torch.float32, device=device)
    n = data_tensor.shape[0]

    best_loss = float("inf")
    best_state = None

    for epoch in range(epochs):
        model.train()
        perm = torch.randperm(n, device=device)
        total_loss = 0.0
        n_batches = 0

        for start in range(0, n, batch_size):
            idx = perm[start: start + batch_size]
            batch = data_tensor[idx]

            mu, logvar, cont, cats = model(batch)
            loss, _ = model.loss(batch, mu, logvar, cont, cats)

            optimizer.zero_grad()
            loss.backward()
            torch.nn.utils.clip_grad_norm_(model.parameters(), max_norm=5.0)
            optimizer.step()

            total_loss += loss.item()
            n_batches += 1

        avg_loss = total_loss / n_batches
        if not np.isnan(avg_loss) and avg_loss < best_loss:
            best_loss = avg_loss
            best_state = copy.deepcopy(model.state_dict())

        if np.isnan(avg_loss):
            print(f"    NaN at epoch {epoch + 1}, restoring best (loss={best_loss:.4f})")
            break

    if best_state is not None:
        model.load_state_dict(best_state)

    return model
