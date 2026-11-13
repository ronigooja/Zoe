from __future__ import annotations

import unittest

from mem0.configs.embeddings.base import BaseEmbedderConfig

from .mem0_embedder import PinnedFastEmbedEmbedding, register_pinned_embedder


class PinnedEmbedderTests(unittest.TestCase):
    def test_mem0_uses_the_worker_pinned_embedder_instance(self) -> None:
        class Encoder:
            dimensions = 384

            @staticmethod
            def encode(text: str) -> list[float]:
                return [float(len(text))] * 384

        encoder = Encoder()
        register_pinned_embedder(encoder)
        adapter = PinnedFastEmbedEmbedding(BaseEmbedderConfig(
            model="sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2",
            embedding_dims=384,
        ))
        self.assertIs(adapter.embedder, encoder)
        self.assertEqual(adapter.embed("abc", "add"), [3.0] * 384)
