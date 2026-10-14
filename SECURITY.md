# Política de seguridad

OpenChuspa guarda datos de otras personas. Los reportes de seguridad tienen prioridad sobre cualquier otra cosa.

## Cómo reportar una vulnerabilidad

**No abra un issue público.** Use el reporte privado de GitHub: pestaña **Security** del repositorio, botón **Report a vulnerability**. El reporte solo lo ven los mantenedores.

Incluya, si puede:

- la versión o el commit afectado;
- los pasos para reproducirlo, o una prueba de concepto;
- el impacto que cree que tiene.

## Qué puede esperar

| Plazo | Compromiso |
|---|---|
| 72 horas | Acuse de recibo |
| 7 días | Evaluación inicial y severidad |
| 90 días como máximo | Corrección publicada y divulgación coordinada con quien reportó |

Cada corrección de seguridad sale con una versión nueva y un aviso de seguridad en GitHub, con crédito a quien la reportó si así lo quiere.

## Versiones con soporte

El proyecto está en `0.x`: solo recibe correcciones la última versión publicada.

## Alcance

Dentro del alcance: saltarse la autenticación o la verificación de firma, leer o escribir fuera de la carpeta de datos, corromper objetos ajenos, agotar los recursos del servidor con peticiones pequeñas, fugas de secretos en logs o respuestas.

Fuera del alcance en esta etapa, porque son límites conocidos y documentados:

- un atacante con acceso de escritura al volumen de datos;
- la falta de cifrado en reposo (planeado para la fase 3);
- exponer el servidor fuera de loopback sin TLS, propio (`CHUSPA_TLS_CERT_FILE`) o de un proxy.

## Verificar lo que ejecuta

Los releases publican la imagen firmada con cosign (sin llaves, con la identidad del workflow de GitHub Actions) y los binarios con sus sumas SHA-256 firmadas. El comando de verificación va en las notas de cada release.
