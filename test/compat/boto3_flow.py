"""Flujo básico con boto3 en su configuración por defecto.

Uso: boto3_flow.py <endpoint> <bucket>. Las credenciales y la región se toman
de las variables de entorno AWS_*.
"""

import hashlib
import os
import sys

import boto3

endpoint, bucket = sys.argv[1], sys.argv[2]
s3 = boto3.client("s3", endpoint_url=endpoint)

# Objeto pequeño, con metadatos
s3.put_object(Bucket=bucket, Key="boto3/pequeño.txt", Body="oíste mira".encode(),
              ContentType="text/plain; charset=utf-8", Metadata={"barrio": "San Antonio"})
got = s3.get_object(Bucket=bucket, Key="boto3/pequeño.txt")
assert got["Body"].read().decode() == "oíste mira"
assert got["ContentType"] == "text/plain; charset=utf-8", got["ContentType"]
assert got["Metadata"] == {"barrio": "San Antonio"}, got["Metadata"]

head = s3.head_object(Bucket=bucket, Key="boto3/pequeño.txt")
assert head["ContentLength"] == len("oíste mira".encode())

# Rango
part = s3.get_object(Bucket=bucket, Key="boto3/pequeño.txt", Range="bytes=0-5")
assert part["Body"].read() == "oíste mira".encode()[:6]
assert part["ResponseMetadata"]["HTTPStatusCode"] == 206

# Multipart por el gestor de transferencias (umbral de 8 MB)
data = os.urandom(20 * 1024 * 1024)
with open("boto3-grande.bin", "wb") as f:
    f.write(data)
s3.upload_file("boto3-grande.bin", bucket, "boto3/grande.bin")
s3.download_file(bucket, "boto3/grande.bin", "boto3-grande.bajado")
with open("boto3-grande.bajado", "rb") as f:
    assert hashlib.sha256(f.read()).digest() == hashlib.sha256(data).digest()

# Multipart a mano, con aborto
upload = s3.create_multipart_upload(Bucket=bucket, Key="boto3/abortada.bin")
s3.upload_part(Bucket=bucket, Key="boto3/abortada.bin", UploadId=upload["UploadId"], PartNumber=1, Body=b"x" * 1024)
parts = s3.list_parts(Bucket=bucket, Key="boto3/abortada.bin", UploadId=upload["UploadId"])
assert [p["PartNumber"] for p in parts["Parts"]] == [1]
s3.abort_multipart_upload(Bucket=bucket, Key="boto3/abortada.bin", UploadId=upload["UploadId"])

# Listado paginado
keys = [o["Key"] for page in s3.get_paginator("list_objects_v2").paginate(
    Bucket=bucket, Prefix="boto3/", PaginationConfig={"PageSize": 1}) for o in page.get("Contents", [])]
assert keys == ["boto3/grande.bin", "boto3/pequeño.txt"], keys

for key in keys:
    s3.delete_object(Bucket=bucket, Key=key)
print("boto3: bien")
