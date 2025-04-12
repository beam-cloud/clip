import struct
import boto3
from dataclasses import dataclass
from botocore.config import Config
from copy import deepcopy
import pygob
from typing import Any

CLIP_ARCHIVE_HEADER_SIZE = 54


@dataclass
class BucketConfiguration:
    bucket: str
    region: str
    endpoint: str
    force_path_style: bool
    access_key: str = None
    secret_key: str = None


"""

type ClipArchiveHeader struct {
        StartBytes            [9]byte
        ClipFileFormatVersion uint8
        IndexLength           int64
        IndexPos              int64
        StorageInfoLength     int64
        StorageInfoPos        int64
        StorageInfoType       [12]byte
}

type S3StorageInfo struct {
        Bucket         string
        Region         string
        Key            string
        Endpoint       string
}

"""


@dataclass
class ClipArchiveHeader:
    start_bytes: bytes
    clip_file_format_version: int
    index_length: int
    index_pos: int
    storage_info_length: int
    storage_info_pos: int
    storage_info_type: bytes


@dataclass
class S3StorageInfo:
    Bucket: bytes
    Region: bytes
    Key: bytes
    Endpoint: bytes


@dataclass
class StorageInfoWrapper:
    Type: bytes
    Data: bytes


class ImageMigrator:
    def __init__(
        self,
        source_bucket: "BucketConfiguration",
        target_bucket: "BucketConfiguration",
    ):
        self.source_bucket: BucketConfiguration = source_bucket
        self.target_bucket: BucketConfiguration = target_bucket
        self.source_s3_client = boto3.client(
            "s3",
            region_name=source_bucket.region,
            endpoint_url=source_bucket.endpoint,
            aws_access_key_id=source_bucket.access_key,
            aws_secret_access_key=source_bucket.secret_key,
            config=Config(
                signature_version="s3v4",
            ),
        )
        self.target_s3_client = boto3.client(
            "s3",
            region_name=target_bucket.region,
            endpoint_url=target_bucket.endpoint,
            aws_access_key_id=target_bucket.access_key,
            aws_secret_access_key=target_bucket.secret_key,
            config=Config(signature_version="s3v4"),
        )

    def migrate(self, image_key: str):
        header: ClipArchiveHeader = self.get_header(image_key)

        if not header.storage_info_type.strip(b"\x00").decode("utf-8") == "s3":
            raise ValueError(f"Unknown storage info type: {header.storage_info_type}")

        s3_info = self.get_s3_storage_info(
            image_key,
            header.storage_info_pos,
            header.storage_info_length,
        )

        print("Original S3 Storage data ==>", s3_info)

        # Create a new instance with modified fields
        new_s3_info = S3StorageInfo(
            Bucket=bytes(self.source_bucket.bucket, "utf-8"),
            Region=bytes(self.source_bucket.region, "utf-8"),
            Key=bytes(s3_info.Key),  # Assuming the Key remains unchanged
            Endpoint=bytes(self.source_bucket.endpoint, "utf-8"),
        )

        # Serialize the S3StorageInfo
        new_s3_info_data = pygob.dump(new_s3_info)

        # Create the StorageInfoWrapper
        new_wrapper = StorageInfoWrapper(Type=b"s3", Data=new_s3_info_data)

        # Serialize the StorageInfoWrapper
        new_wrapper_data = pygob.dump(new_wrapper)
        print("New Wrapper Data ==>", new_wrapper_data)

        print("New Wrapper Data ==>", pygob.load(new_wrapper_data))
        new_storage_info_length = len(new_wrapper_data)

        # Update the header with the new storage info length
        header.storage_info_length = new_storage_info_length

        # Download the entire archive
        full_data = self.download_full_archive(image_key)

        # Update the binary with the new header and storage info
        updated_data = self.update_binary(full_data, header, new_wrapper_data)

        # Upload the updated binary to the target bucket
        self.upload_to_target_bucket(image_key, updated_data)

    def get_header(self, image_key: str) -> ClipArchiveHeader:
        response = self.source_s3_client.get_object(
            Bucket=self.source_bucket.bucket, Key=image_key
        )

        # Read the entire header at once
        data = response["Body"].read(CLIP_ARCHIVE_HEADER_SIZE)

        # Unpack the data according to the ClipArchiveHeader structure
        try:
            unpacked_data = struct.unpack("<9sBqqqq12s", data)
        except struct.error as e:
            print(f"Unpacking error: {e}")

        header = ClipArchiveHeader(
            start_bytes=unpacked_data[0],
            clip_file_format_version=unpacked_data[1],
            index_length=unpacked_data[2],
            index_pos=unpacked_data[3],
            storage_info_length=unpacked_data[4],
            storage_info_pos=unpacked_data[5],
            storage_info_type=unpacked_data[6],
        )

        return header

    def get_s3_storage_info(self, image_key: str, pos: int, length: int) -> Any:
        response = self.source_s3_client.get_object(
            Bucket=self.source_bucket.bucket,
            Key=image_key,
            Range=f"bytes={pos}-{pos + length - 1}",
        )
        data = response["Body"].read(length)
        print("Data ==>", data)

        wrapper = pygob.load(data)
        print("Wrapper ==>", wrapper)
        s3_info_data = wrapper.Data
        s3_info = pygob.load(s3_info_data)
        return s3_info

    def download_full_archive(self, image_key: str) -> bytes:
        response = self.source_s3_client.get_object(
            Bucket=self.source_bucket.bucket, Key=image_key
        )
        return response["Body"].read()

    def update_binary(
        self, full_data: bytes, header: ClipArchiveHeader, new_s3_info_data: bytes
    ) -> bytes:
        # Pack the updated header
        updated_header = struct.pack(
            "<9sBqqqq12s",
            header.start_bytes,
            header.clip_file_format_version,
            header.index_length,
            header.index_pos,
            header.storage_info_length,
            header.storage_info_pos,
            header.storage_info_type,
        )

        # Replace the old header and storage info in the binary
        updated_data = (
            updated_header
            + full_data[CLIP_ARCHIVE_HEADER_SIZE : header.storage_info_pos]
            + new_s3_info_data
            + full_data[header.storage_info_pos + header.storage_info_length :]
        )

        return updated_data

    def upload_to_target_bucket(self, image_key: str, data: bytes):
        self.target_s3_client.put_object(
            Bucket=self.target_bucket.bucket, Key=image_key, Body=data
        )


if __name__ == "__main__":
    source_bucket = BucketConfiguration(
        bucket="beta9-images-stage-ce9b32841953",
        region="auto",
        endpoint="https://fly.storage.tigris.dev",
        force_path_style=False,
        access_key="tid_KhmxXXsacqRarGwaQypPBMxIhzrIivrDo_OvncPwoOtzycvhz_",
        secret_key="tsec_97W+nPbPA-WHv_YuLe-JVOcY8HL_ZWxmXzTW444zs-rWvyp5_5fPz0e57lNKgFdBhdSWda",
    )

    target_bucket = BucketConfiguration(
        bucket="beta9-images-stage-ce9b32841953",
        region="us-east-1",
        endpoint="https://s3.wasabisys.com",
        force_path_style=False,
        access_key="G904AT43487164J9QEO7",
        secret_key="U7ZeK3zkkON0l48iHIS1BmQyK8LyHndkpmyNBKwj",
    )

    migrator = ImageMigrator(source_bucket, target_bucket)
    migrator.migrate("00a9ff88327900f6.rclip")
