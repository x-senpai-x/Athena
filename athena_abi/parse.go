package athena_abi

import (
	"fmt"
	"strings"
)

// Groups ABI JSON by ABI Type. If type is 'struct' or 'enum', it is grouped as a 'type_def'
func GroupAbiByType(abiJson []map[string]interface{}) map[AbiMemberType][]map[string]interface{} {
	grouped := make(map[AbiMemberType][]map[string]interface{})

	for _, entry := range abiJson {
		if entry["type"] == "struct" || entry["type"] == "enum" {
			grouped["type_def"] = append(grouped["type_def"], entry)
		} else {
			grouped[entry["type"].(AbiMemberType)] = append(grouped[entry["type"].(AbiMemberType)], entry)
		}
	}
	return grouped
}

// Non-Struct Defined Types
// Used for Topological Sorting abi struct and enum definitions of incorrectly ordered abis
var StarknetCoreTypes = map[string]struct{}{
	"felt":                {}, // Old Syntax for core::felt252
	"felt*":               {}, // Old Syntax for arrays
	"core::integer::u128": {},
	"core::integer::u64":  {},
	"core::integer::u32":  {},
	"core::integer::u16":  {},
	"core::integer::u8":   {},
	"core::felt252":       {},
	"core::bool":          {},
	"core::starknet::contract_address::ContractAddress": {},
	"core::starknet::class_hash::ClassHash":             {},
	"core::starknet::eth_address::EthAddress":           {},
}

func extractInnerType(abiType string) string {
	start := strings.Index(abiType, "<")
	end := strings.LastIndex(abiType, ">")

	if start == -1 || end == -1 || start+1 >= end {
		return abiType
	}

	return abiType[start+1 : end]
}

// The function takes in a list of type definitions (dict) and returns a dict of sets (map[string]bool)
func BuildTypeGraph(typeDefs []map[string]interface{}) map[string]map[string]bool {
	outputGraph := make(map[string]map[string]bool)
	for _, typeDef := range typeDefs {
		referencedTypes := []string{}
		if typeDef["type"] == "struct" {
			for _, member := range typeDef["members"].([]map[string]interface{}) {
				referencedTypes = append(referencedTypes, member["type"].(string))
			}
		} else {
			for _, variant := range typeDef["variants"].([]map[string]interface{}) {
				referencedTypes = append(referencedTypes, variant["type"].(string))
			}
		}

		refTypes := make(map[string]bool)

		for _, typeStr := range referencedTypes {
			if _, ok := StarknetCoreTypes[typeStr]; ok {
				continue
			}

			if _, ok := StarknetCoreTypes[extractInnerType(typeStr)]; ok {
				if strings.HasPrefix(typeStr, "core::array") || strings.HasPrefix(typeStr, "@core::array") {
					continue
				}
			}

			refTypes[typeStr] = true
		}

		outputGraph[typeDef["name"].(string)] = refTypes
	}

	return outputGraph
}

func TopoSortTypeDefs(typeDefs []map[string]interface{}) ([]map[string]interface{}, error) {
	typeGraph := BuildTypeGraph(typeDefs)
	sortedDefs := TopologicalSort(convertMap(typeGraph))

	sortedTypeDefJson := []map[string]interface{}{}

	for _, sortedTypeName := range sortedDefs {
		abiDefinition := []map[string]interface{}{}
		for _, typeDef := range typeDefs {
			if typeDef["name"] == sortedTypeName {
				abiDefinition = append(abiDefinition, typeDef)
			}
		}
		if len(abiDefinition) == 0 {
			return nil, &InvalidAbiError{
				Msg: "Type " + sortedTypeName + " not defined in ABI",
			}
		}
		if len(abiDefinition) > 1 {
			return nil, &InvalidAbiError{
				Msg: "Type " + sortedTypeName + " defined multiple times in ABI",
			}
		}
		sortedTypeDefJson = append(sortedTypeDefJson, abiDefinition[0])
	}
	return sortedTypeDefJson, nil
}

// Parses an **ordered** array of ABI structs into a dictionary of StarknetStructs, mapping struct name to struct.
// return value is a map from string to StarknetStruct or StarknetEnum
func ParseEnumsAndStructs(abiStructs []map[string]interface{}) (map[string]interface{}, error) {
	outputTypes := make(map[string]interface{})

	for _, abiStruct := range abiStructs {
		typeName := abiStruct["name"].(string)
		typeParts := strings.Split(typeName, "::")

		switch {
		case typeName == "Uint256":
			continue

		case len(typeParts) > 1 && (typeParts[0] == "core" || typeParts[0] == "@core") &&
			(typeParts[1] == "array" || typeParts[1] == "integer" || typeParts[1] == "bool" || typeParts[1] == "option" || typeParts[1] == "zeroable"):
			continue

		}

		switch abiStruct["type"] {
		case "struct":
			res, err := parseStruct(abiStruct, outputTypes)
			if err != nil {
				return nil, err
			}
			outputTypes[typeName] = res

		case "enum":
			res, err := parseEnum(abiStruct, outputTypes)
			if err != nil {
				return nil, err
			}
			outputTypes[typeName] = res
		}
	}

	return outputTypes, nil
}

func parseStruct(abiStruct map[string]interface{}, typeContext map[string]interface{}) (StarknetStruct, error) {
	members := []AbiParameter{}

	for _, member := range abiStruct["members"].([]map[string]interface{}) {
		res, err := parseType(member["type"].(string), typeContext)
		if err != nil {
			return StarknetStruct{}, err
		}
		members = append(members, AbiParameter{
			Name: member["name"].(string),
			Type: res,
		})
	}

	return StarknetStruct{
		Name:    abiStruct["name"].(string),
		Members: members,
	}, nil
}

func parseEnum(abiEnum map[string]interface{}, typeContext map[string]interface{}) (StarknetEnum, error) {
	variants := []struct {
		Name string
		Type StarknetType
	}{}

	for _, variant := range abiEnum["variants"].([]map[string]interface{}) {
		res, err := parseType(variant["type"].(string), typeContext)
		if err != nil {
			return StarknetEnum{}, err
		}
		variants = append(variants, struct {
			Name string
			Type StarknetType
		}{
			Name: variant["name"].(string),
			Type: res,
		})
	}

	return StarknetEnum{
		Name:     abiEnum["name"].(string),
		Variants: variants,
	}, nil
}

func parseType(abiType string, customTypes map[string]interface{}) (StarknetType, error) {
	if abiType == "()" {
		return NoneType, nil
	}

	if strings.HasPrefix(abiType, "(") {
		res, err := ParseTuple(abiType, customTypes)
		if err != nil {
			return nil, err
		}
		return res, nil
	}

	parts := strings.Split(abiType, "::")[1:]
	switch {
	case len(parts) == 1 && parts[0] == "felt252":
		return Felt, nil
	case len(parts) == 1 && parts[0] == "bool":
		return Bool, nil
	case len(parts) == 3 && parts[0] == "starknet" && parts[1] == "contract_address" && parts[2] == "ContractAddress":
		return ContractAddress, nil
	case len(parts) == 3 && parts[0] == "starknet" && parts[1] == "class_hash" && parts[2] == "ClassHash":
		return ClassHash, nil
	case len(parts) == 3 && parts[0] == "starknet" && parts[1] == "eth_address" && parts[2] == "EthAddress":
		return EthAddress, nil
	case len(parts) == 2 && parts[0] == "bytes_31" && parts[1] == "bytes31":
		return Bytes31, nil
	case len(parts) == 3 && parts[0] == "starknet" && parts[1] == "storage_access" && parts[2] == "StorageAddress":
		return StorageAddress, nil
	case len(parts) >= 2 && parts[0] == "array" && parts[1] == "Array" || parts[1] == "Span":
		res, err := parseType(extractInnerType(abiType), customTypes)
		if err != nil {
			return nil, err
		}
		return StarknetArray{res}, nil
	case len(parts) >= 2 && parts[0] == "option" && parts[1] == "Option":
		res, err := parseType(extractInnerType(abiType), customTypes)
		if err != nil {
			return nil, err
		}
		return StarknetOption{res}, nil
	case len(parts) >= 2 && parts[0] == "zeroable" && parts[1] == "NonZero":
		res, err := parseType(extractInnerType(abiType), customTypes)
		if err != nil {
			return nil, err
		}
		return StarknetNonZero{res}, nil
	default:
		if val, exists := customTypes[abiType]; exists {
			return val.(StarknetType), nil
		}
		if abiType == "felt" {
			return Felt, nil
		}
		if abiType == "Uint256" {
			return U256, nil
		}
		if strings.HasSuffix(abiType, "*") {
			res, err := parseType(strings.TrimSuffix(abiType, "*"), customTypes)
			if err != nil {
				return nil, err
			}
			return StarknetArray{res}, nil
		}
		return nil, &InvalidAbiError{
			Msg: "Invalid ABI type: " + abiType,
		}
	}
}

func isNamedTuple(typeStr string) int {
	for i := 1; i < len(typeStr)-1; i++ {
		if typeStr[i] == ':' && typeStr[i-1] != ':' && typeStr[i+1] != ':' {
			return i
		}
	}
	if len(typeStr) > 1 && typeStr[0] == ':' && typeStr[1] != ':' {
		return 0
	}
	if len(typeStr) > 1 && typeStr[len(typeStr)-1] == ':' && typeStr[len(typeStr)-2] != ':' {
		return len(typeStr) - 1
	}
	return -1
}

// customTypes is a map from string to StarknetStruct or StarknetEnum
func ParseTuple(abiType string, customTypes map[string]interface{}) (StarknetTuple, error) {
	strippedTuple := strings.TrimSpace(abiType[1 : len(abiType)-1])
	outputTypes := []StarknetType{}
	parenthesisCache := []string{}
	typeCache := []string{}
	for _, typeString := range strings.Split(strippedTuple, ",") {
		tupleOpen := strings.Count(typeString, "(")
		tupleClose := strings.Count(typeString, ")")

		if tupleOpen > 0 {
			for i := 0; i < tupleOpen; i++ {
				parenthesisCache = append(parenthesisCache, "(")
			}
		}

		if len(parenthesisCache) > 0 {
			typeCache = append(typeCache, typeString)
		} else {
			if isNamedTuple(typeString) > 0 {
				res, err := parseType(typeString[isNamedTuple(typeString)+1:], customTypes)
				if err != nil {
					return StarknetTuple{}, err
				}
				outputTypes = append(outputTypes, res)
			} else {
				res, err := parseType(typeString, customTypes)
				if err != nil {
					return StarknetTuple{}, err
				}
				outputTypes = append(outputTypes, res)
			}
		}

		if tupleClose > 0 {
			parenthesisCache = parenthesisCache[:len(parenthesisCache)-tupleClose]
			if len(parenthesisCache) == 0 {
				res, err := ParseTuple(strings.Join(typeCache, ","), customTypes)
				if err != nil {
					return StarknetTuple{}, err
				}
				outputTypes = append(outputTypes, res)
			}
		}
	}
	return StarknetTuple{Members: outputTypes}, nil
}

func parseAbiParameters(names []string, types []string, customTypes map[string]interface{}) ([]AbiParameter, error) {
	outputParameters := []AbiParameter{}

	for i := 0; i < len(names); i++ {
		if strings.HasSuffix(types[i], "*") {
			lenParam := outputParameters[len(outputParameters)-1]
			outputParameters = outputParameters[:len(outputParameters)-1]
			if !(strings.HasSuffix(lenParam.Name, "_len") || strings.HasSuffix(lenParam.Name, "_size")) {
				return nil, fmt.Errorf("Type " + types[i] + " not preceded by a length parameter")
			}
		}

		res, err := parseType(types[i], customTypes)
		if err != nil {
			return nil, err
		}
		outputParameters = append(outputParameters, AbiParameter{
			Name: names[i],
			Type: res,
		})
	}

	return outputParameters, nil
}

func ParseAbiTypes(types []string, customTypes map[string]interface{}) ([]StarknetType, error) {
	outputTypes := []StarknetType{}

	for _, jsonTypeStr := range types {
		if strings.HasSuffix(jsonTypeStr, "*") {
			lenType := outputTypes[len(outputTypes)-1]
			outputTypes = outputTypes[:len(outputTypes)-1]
			if lenType != Felt {
				return nil, fmt.Errorf("Type " + jsonTypeStr + " not preceded by a Felt Length Param")
			}
		}

		res, err := parseType(jsonTypeStr, customTypes)
		if err != nil {
			return nil, err
		}
		outputTypes = append(outputTypes, res)
	}

	return outputTypes, nil
}

func ParseAbiFunction(abiFunction map[string]interface{}, customTypes map[string]interface{}) (*AbiFunction, error) {
	names := []string{}
	types := []string{}
	for _, abiInput := range abiFunction["inputs"].([]map[string]interface{}) {
		names = append(names, abiInput["name"].(string))
	}
	for _, abiInput := range abiFunction["inputs"].([]map[string]interface{}) {
		types = append(types, abiInput["type"].(string))
	}
	parsedInputs, err := parseAbiParameters(
		names,
		types,
		customTypes,
	)
	if err != nil {
		return nil, err
	}

	for _, abiOutput := range abiFunction["outputs"].([]map[string]interface{}) {
		types = append(types, abiOutput["type"].(string))
	}

	parsedOutputs, err := ParseAbiTypes(
		types,
		customTypes,
	)
	if err != nil {
		return nil, err
	}

	return &AbiFunction{
		name:    abiFunction["name"].(string),
		inputs:  parsedInputs,
		outputs: parsedOutputs,
	}, nil
}

func ParseAbiEvent(abiEvent map[string]interface{}, customTypes map[string]interface{}) (*AbiEvent, error) {
	eventParameters := []map[string]interface{}{}
	if value, exists := abiEvent["kind"]; exists {
		if value == "struct" {
			eventParameters = abiEvent["members"].([]map[string]interface{})
		} else {
			return nil, nil
		}
	} else if inputs, ok := abiEvent["inputs"].([]map[string]interface{}); ok {
		for _, e := range inputs {
			eventParameter := map[string]interface{}{"kind": "data"}
			for k, v := range e {
				eventParameter[k] = v
			}
			eventParameters = append(eventParameters, eventParameter)
		}
	} else if data, ok := abiEvent["data"].([]map[string]interface{}); ok {
		for _, e := range data {
			eventParameter := map[string]interface{}{"kind": "data"}
			for k, v := range e {
				eventParameter[k] = v
			}
			eventParameters = append(eventParameters, eventParameter)
		}
		for _, e := range abiEvent["keys"].([]map[string]interface{}) {
			eventParameter := map[string]interface{}{"kind": "key"}
			for k, v := range e {
				eventParameter[k] = v
			}
			eventParameters = append(eventParameters, eventParameter)
		}
	} else {
		return nil, nil
	}

	types := []string{}
	names := []string{}

	for _, eventParameter := range eventParameters {
		types = append(types, eventParameter["type"].(string))
		names = append(names, eventParameter["name"].(string))
	}

	decodedParams, err := parseAbiParameters(
		names,
		types,
		customTypes,
	)

	if err != nil {
		return nil, err
	}

	eventKinds := map[string]string{}
	for _, eventParameter := range eventParameters {
		eventKinds[eventParameter["name"].(string)] = eventParameter["kind"].(string)
	}

	eventData := map[string]StarknetType{}
	for _, param := range decodedParams {
		if eventKinds[param.Name] == "data" {
			eventData[param.Name] = param.Type
		}
	}

	eventKeys := map[string]StarknetType{}
	for _, param := range decodedParams {
		if eventKinds[param.Name] == "key" {
			eventKeys[param.Name] = param.Type
		}
	}

	parts := strings.Split(abiEvent["name"].(string), "::")

	abiEventParams := []string{}

	for _, param := range decodedParams {
		abiEventParams = append(abiEventParams, param.Name)
	}

	return &AbiEvent{
		name:       parts[len(parts)-1],
		parameters: abiEventParams,
		data:       eventData,
		keys:       eventKeys,
	}, nil
}

// ---- Notes ----
//   When the event is emitted, the serialization to keys and data happens as follows:

//   Since the TestEnum variant has kind nested, add the first key: sn_keccak(TestEnum),
//   and the rest of the serialization to keys and data is done recursively via
//   the starknet::event trait implementation of MyEnum.

//   Next, you can handle a "kind": "nested" variant (previously it was TestEnum, now it’s Var1),
//   which means you can add another key depending on the sub-variant: sn_keccak(Var1), and proceed
//   to serialize according to the starknet::event implementation of MyStruct.
//
//   Finally, proceed to serialize MyStruct, which gives us a single data member.
//
//   This results in keys = [sn_keccak(TestEnum), sn_keccak(Var1)] and data=[5]
