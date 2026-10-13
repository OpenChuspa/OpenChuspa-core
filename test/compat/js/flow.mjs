// Flujo básico con @aws-sdk/client-s3 en su configuración por defecto, salvo
// el estilo de ruta.
//
// Uso: node flow.mjs <endpoint> <bucket>. Credenciales y región por variables AWS_*.
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { Readable } from "node:stream";
import {
  CopyObjectCommand,
  DeleteObjectsCommand,
  GetObjectCommand,
  HeadObjectCommand,
  ListObjectsV2Command,
  PutObjectCommand,
  S3Client,
} from "@aws-sdk/client-s3";
import { Upload } from "@aws-sdk/lib-storage";

const [endpoint, Bucket] = process.argv.slice(2);
const s3 = new S3Client({ endpoint, forcePathStyle: true });
const bytes = async (body) => Buffer.from(await body.transformToByteArray());

// Objeto pequeño con metadatos
const small = Buffer.from("oíste mira");
await s3.send(new PutObjectCommand({
  Bucket, Key: "js/pequeño.txt", Body: small, ContentType: "text/plain", Metadata: { barrio: "San Antonio" },
}));
let got = await s3.send(new GetObjectCommand({ Bucket, Key: "js/pequeño.txt" }));
assert.deepEqual(await bytes(got.Body), small);
assert.deepEqual(got.Metadata, { barrio: "San Antonio" });

// Cuerpo como stream: el SDK lo envía en aws-chunked con el checksum de trailer
const streamed = randomBytes(256 * 1024);
await s3.send(new PutObjectCommand({
  Bucket, Key: "js/stream.bin", Body: Readable.from([streamed.subarray(0, 100000), streamed.subarray(100000)]),
  ContentLength: streamed.length,
}));
got = await s3.send(new GetObjectCommand({ Bucket, Key: "js/stream.bin" }));
assert.deepEqual(await bytes(got.Body), streamed);

// Rango
got = await s3.send(new GetObjectCommand({ Bucket, Key: "js/pequeño.txt", Range: "bytes=0-5" }));
assert.deepEqual(await bytes(got.Body), small.subarray(0, 6));

// Multipart con lib-storage
const big = randomBytes(20 * 1024 * 1024);
await new Upload({ client: s3, params: { Bucket, Key: "js/grande.bin", Body: big } }).done();
got = await s3.send(new GetObjectCommand({ Bucket, Key: "js/grande.bin" }));
assert.ok((await bytes(got.Body)).equals(big), "el archivo grande no volvió idéntico");

// Copia
await s3.send(new CopyObjectCommand({ Bucket, Key: "js/copia.txt", CopySource: `${Bucket}/js/peque%C3%B1o.txt` }));

// Listado paginado
const keys = [];
let ContinuationToken;
do {
  const page = await s3.send(new ListObjectsV2Command({ Bucket, Prefix: "js/", MaxKeys: 1, ContinuationToken }));
  keys.push(...(page.Contents ?? []).map((o) => o.Key));
  ContinuationToken = page.NextContinuationToken;
} while (ContinuationToken);
assert.deepEqual(keys, ["js/copia.txt", "js/grande.bin", "js/pequeño.txt", "js/stream.bin"]);

// Error tipado
await assert.rejects(s3.send(new HeadObjectCommand({ Bucket, Key: "js/no-existe" })), { name: "NotFound" });

// Borrado masivo
const res = await s3.send(new DeleteObjectsCommand({ Bucket, Delete: { Objects: keys.map((Key) => ({ Key })) } }));
assert.equal(res.Deleted.length, keys.length);
assert.equal(res.Errors, undefined);

console.log("@aws-sdk/client-s3: bien");
