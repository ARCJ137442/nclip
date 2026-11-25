export function uploadAuthMessage(status, code, hasUploadKeyField) {
  if (status !== 401 || !hasUploadKeyField) return null;
  if (code === "missing api key") return "uploadKeyRequired";
  if (code === "unauthorized") return "uploadKeyInvalid";
  return null;
}
