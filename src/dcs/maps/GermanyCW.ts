import GermanyCWAirBases from "../../data/airbases/germanycw.json";
import { convertRawAirBaseData, DCSMap } from "./DCSMap";

export const GermanyCW: DCSMap = {
  name: "GermanyCW",
  center: [52.0, 10.5],
  magDec: 3,
  airports: convertRawAirBaseData(GermanyCWAirBases),
};
