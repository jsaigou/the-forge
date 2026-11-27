// Which model cards stand for something served only by external providers (no local config at all). Checked against the whole catalog, not a
// card's visible configs: a model with only hidden local configs is still hosted here. Answers false until the catalog has loaded.
import { useCatalogConfigs, useCatalogModels, useCatalogOfferings, useCatalogVariants } from "./queries";

export function useRemoteOnly(): (modelName: string) => boolean {
  const models = useCatalogModels();
  const offerings = useCatalogOfferings();
  const configs = useCatalogConfigs();
  const variants = useCatalogVariants();
  return (modelName) => {
    const model = models.data?.find((m) => m.name === modelName);
    if (!model || !offerings.data || !configs.data || !variants.data) return false;
    if (!offerings.data.some((o) => o.model_id === model.id)) return false;
    return !configs.data.some((c) => variants.data.some((v) => v.id === c.variant_id && v.model_id === model.id));
  };
}
