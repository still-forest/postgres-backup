"""Object storage abstraction and its Cloudflare R2 implementation."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import BinaryIO, Protocol

import boto3
from boto3.s3.transfer import TransferConfig
from botocore.config import Config


@dataclass(frozen=True)
class Object:
    """One stored backup artifact."""

    key: str
    size: int
    last_modified: datetime


class ObjectStore(Protocol):
    """The subset of object storage this service needs.

    ``list_objects`` must never return a partially-enumerated result: callers use
    it to decide what to delete, so a truncated listing that looked complete
    would cause data loss. Implementations let any paging error propagate.
    """

    def upload(self, key: str, body: BinaryIO, metadata: dict[str, str]) -> None: ...

    def list_objects(self, prefix: str) -> list[Object]: ...

    def download(self, key: str, dest: BinaryIO) -> None: ...

    def head(self, key: str) -> tuple[Object, dict[str, str]]: ...

    def delete(self, key: str) -> None: ...


class R2Store:
    """An ObjectStore backed by Cloudflare R2 over the S3 API."""

    def __init__(
        self,
        *,
        access_key_id: str,
        secret_access_key: str,
        bucket: str,
        host_base: str,
    ) -> None:
        self._bucket = bucket
        self._client = boto3.session.Session().client(
            "s3",
            endpoint_url=host_base,
            aws_access_key_id=access_key_id,
            aws_secret_access_key=secret_access_key,
            # R2 has no regions; the SDK requires one for SigV4 signing and
            # "auto" is what Cloudflare specifies. The DEFAULT_REGION the
            # previous container passed ("apac") is a jurisdiction hint used at
            # bucket creation, never a signing region.
            region_name="auto",
            config=Config(
                # R2 rejects requests carrying more than one non-default
                # checksum, which is what the SDK sends by default.
                request_checksum_calculation="when_required",
                response_checksum_validation="when_required",
                # R2's certificate covers *.r2.cloudflarestorage.com, so
                # virtual-host addressing would produce a two-label subdomain
                # the wildcard does not cover.
                s3={"addressing_style": "path"},
                retries={"max_attempts": 5, "mode": "standard"},
            ),
        )
        self._transfer = TransferConfig(
            multipart_threshold=16 * 1024 * 1024,
            multipart_chunksize=16 * 1024 * 1024,
            max_concurrency=4,
        )

    def upload(self, key: str, body: BinaryIO, metadata: dict[str, str]) -> None:
        """Stream ``body`` to ``key``. Multipart is handled by the transfer manager."""
        self._client.upload_fileobj(
            body,
            self._bucket,
            key,
            # ChecksumAlgorithm is deliberately unset; combined with
            # request_checksum_calculation="when_required", that is what keeps
            # R2 happy.
            ExtraArgs={"Metadata": metadata} if metadata else None,
            Config=self._transfer,
        )

    def list_objects(self, prefix: str) -> list[Object]:
        """Enumerate every object under ``prefix``.

        Any paging error propagates, aborting the whole listing. Callers use the
        result to decide what to delete, so returning a partial list that looked
        complete would cause data loss.
        """
        paginator = self._client.get_paginator("list_objects_v2")
        out: list[Object] = []
        for page in paginator.paginate(Bucket=self._bucket, Prefix=prefix):
            for item in page.get("Contents", []):
                out.append(
                    Object(
                        key=item["Key"],
                        size=int(item["Size"]),
                        last_modified=item["LastModified"],
                    )
                )
        return out

    def download(self, key: str, dest: BinaryIO) -> None:
        self._client.download_fileobj(self._bucket, key, dest, Config=self._transfer)

    def head(self, key: str) -> tuple[Object, dict[str, str]]:
        resp = self._client.head_object(Bucket=self._bucket, Key=key)
        obj = Object(
            key=key,
            size=int(resp["ContentLength"]),
            last_modified=resp["LastModified"],
        )
        return obj, dict(resp.get("Metadata", {}))

    def delete(self, key: str) -> None:
        self._client.delete_object(Bucket=self._bucket, Key=key)

    def check_access(self) -> None:
        """Verify the credentials can reach the bucket, creating nothing.

        The container this replaces ran ``s3cmd ls || s3cmd mb``, which turned a
        bucket-scoped key into an attempted bucket creation and a fatal error.
        """
        try:
            self._client.head_bucket(Bucket=self._bucket)
        except Exception as exc:
            raise RuntimeError(
                f"bucket {self._bucket!r} is not reachable with the configured credentials: {exc}"
            ) from exc
