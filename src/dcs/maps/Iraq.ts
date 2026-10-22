import IraqAirBases from "../../data/airbases/iraq.json";
import { convertRawAirBaseData, DCSMap } from "./DCSMap";

export const Iraq: DCSMap = {
  name: "Iraq",
  center: [33.5, 43.5],
  magDec: 5,
  airports: convertRawAirBaseData(IraqAirBases),
};
